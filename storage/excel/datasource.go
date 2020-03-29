package excel

import (
	"context"
	"log"

	"github.com/360EntSecGroup-Skylar/excelize"
	"github.com/pkg/errors"

	"github.com/cube2222/octosql"
	"github.com/cube2222/octosql/config"
	"github.com/cube2222/octosql/execution"
	"github.com/cube2222/octosql/physical"
	"github.com/cube2222/octosql/physical/metadata"
	"github.com/cube2222/octosql/streaming/storage"
)

var availableFilters = map[physical.FieldType]map[physical.Relation]struct{}{
	physical.Primary:   make(map[physical.Relation]struct{}),
	physical.Secondary: make(map[physical.Relation]struct{}),
}

type DataSource struct {
	path             string
	alias            string
	hasHeaderRow     bool
	sheet            string
	timeColumns      []string
	horizontalOffset int
	verticalOffset   int
	batchSize        int
	stateStorage     storage.Storage
}

func NewDataSourceBuilderFactory() physical.DataSourceBuilderFactory {
	return physical.NewDataSourceBuilderFactory(
		func(ctx context.Context, matCtx *physical.MaterializationContext, dbConfig map[string]interface{}, filter physical.Formula, alias string, partition int) (execution.Node, error) {
			path, err := config.GetString(dbConfig, "path")
			if err != nil {
				return nil, errors.Wrap(err, "couldn't get path")
			}

			hasHeaderRow, err := config.GetBool(dbConfig, "headerRow", config.WithDefault(true))
			if err != nil {
				return nil, errors.Wrap(err, "couldn't get header row option")
			}

			sheet, err := config.GetString(dbConfig, "sheet", config.WithDefault("Sheet1"))
			if err != nil {
				return nil, errors.Wrap(err, "couldn't get sheet name")
			}

			rootCell, err := config.GetString(dbConfig, "rootCell", config.WithDefault("A1"))
			if err != nil {
				return nil, errors.Wrap(err, "couldn't get root cell")
			}

			timeColumns, err := config.GetStringList(dbConfig, "timeColumns", config.WithDefault([]string{}))
			if err != nil {
				return nil, errors.Wrap(err, "couldn't get time columns")
			}

			verticalOffset, horizontalOffset, err := getCoordinatesFromCell(rootCell)
			if err != nil {
				return nil, errors.Wrap(err, "couldn't extract column and row numbers from root cell")
			}

			batchSize, err := config.GetInt(dbConfig, "batchSize", config.WithDefault(1000))
			if err != nil {
				return nil, errors.Wrap(err, "couldn't get batch size")
			}

			return &DataSource{
				path:             path,
				alias:            alias,
				hasHeaderRow:     hasHeaderRow,
				sheet:            sheet,
				timeColumns:      timeColumns,
				horizontalOffset: horizontalOffset,
				verticalOffset:   verticalOffset,
				batchSize:        batchSize,
				stateStorage:     matCtx.Storage,
			}, nil
		},
		nil,
		availableFilters,
		metadata.BoundedFitsInLocalStorage,
		1,
	)
}

// NewDataSourceBuilderFactoryFromConfig creates a data source builder factory using the configuration.
func NewDataSourceBuilderFactoryFromConfig(dbConfig map[string]interface{}) (physical.DataSourceBuilderFactory, error) {
	return NewDataSourceBuilderFactory(), nil
}

func (ds *DataSource) Get(ctx context.Context, variables octosql.Variables, streamID *execution.StreamID) (execution.RecordStream, *execution.ExecutionOutput, error) {
	rs := &RecordStream{
		stateStorage:     ds.stateStorage,
		streamID:         streamID,
		first:            true,
		filePath:         ds.path,
		sheet:            ds.sheet,
		hasHeaderRow:     ds.hasHeaderRow,
		timeColumnNames:  ds.timeColumns,
		alias:            ds.alias,
		horizontalOffset: ds.horizontalOffset,
		verticalOffset:   ds.verticalOffset,
		isDone:           false,
		batchSize:        ds.batchSize,
	}

	go func() {
		log.Println("worker start")
		rs.RunWorker(ctx)
		log.Println("worker done")
	}()

	return rs, execution.NewExecutionOutput(execution.NewZeroWatermarkGenerator()), nil
}

type RecordStream struct {
	stateStorage     storage.Storage
	streamID         *execution.StreamID
	first            bool
	filePath         string
	sheet            string
	hasHeaderRow     bool
	timeColumnNames  []string
	alias            string
	horizontalOffset int
	verticalOffset   int

	isDone      bool
	columnNames []octosql.VariableName
	timeColumns []bool
	rows        *excelize.Rows
	offset      int
	batchSize   int
}

func (rs *RecordStream) Close() error {
	return nil
}

func (rs *RecordStream) RunWorker(ctx context.Context) {
	for { // outer for is loading offset value and moving file iterator
		tx := rs.stateStorage.BeginTransaction().WithPrefix(rs.streamID.AsPrefix())

		if err := rs.loadOffset(tx); err != nil {
			log.Fatalf("excel worker: couldn't reinitialize offset for excel read batch worker: %s", err)
			return
		}

		tx.Abort() // We only read data above, no need to risk failing now.

		// Load/Reload file
		file, err := excelize.OpenFile(rs.filePath)
		if err != nil {
			log.Fatalf("excel worker: couldn't open file: %s", err)
			return
		}

		rows, err := file.Rows(rs.sheet)
		if err != nil {
			log.Fatalf("excel worker: couldn't get sheet's rows: %s", err)
			return
		}

		for i := 0; i <= rs.verticalOffset; i++ {
			if !rows.Next() {
				log.Fatalf("excel worker: root cell is lower than row count: %s", err)
				return
			}
		}

		rs.rows = rows

		// Moving file iterator by `rs.offset`
		for i := 0; i < rs.offset; i++ {
			_, err := rs.readRecordFromFileWithInitialize()
			if err == execution.ErrEndOfStream {
				return
			} else if err != nil {
				log.Fatalf("excel worker: couldn't move excel file iterator by %d offset: %s", rs.offset, err)
				return
			}
		}

		for { // inner for is calling RunWorkerInternal
			tx := rs.stateStorage.BeginTransaction().WithPrefix(rs.streamID.AsPrefix())

			err := rs.RunWorkerInternal(ctx, tx)
			if errors.Cause(err) == execution.ErrNewTransactionRequired {
				tx.Abort()
				continue
			} else if waitableError := execution.GetErrWaitForChanges(err); waitableError != nil {
				tx.Abort()
				err = waitableError.ListenForChanges(ctx)
				if err != nil {
					log.Println("excel worker: couldn't listen for changes: ", err)
				}
				err = waitableError.Close()
				if err != nil {
					log.Println("excel worker: couldn't close storage changes subscription: ", err)
				}
				continue
			} else if err == execution.ErrEndOfStream {
				err = tx.Commit()
				if err != nil {
					log.Println("excel worker: couldn't commit transaction: ", err)
					continue
				}
				return
			} else if err != nil {
				tx.Abort()
				log.Printf("excel worker: error running excel read batch worker: %s, reinitializing from storage", err)
				break
			}

			err = tx.Commit()
			if err != nil {
				log.Println("excel worker: couldn't commit transaction: ", err)
				continue
			}
		}
	}
}

var outputQueuePrefix = []byte("$output_queue$")

func (rs *RecordStream) RunWorkerInternal(ctx context.Context, tx storage.StateTransaction) error {
	outputQueue := execution.NewOutputQueue(tx.WithPrefix(outputQueuePrefix))

	if rs.isDone {
		err := outputQueue.Push(ctx, &QueueElement{
			Type: &QueueElement_EndOfStream{
				EndOfStream: true,
			},
		})
		if err != nil {
			return errors.Wrapf(err, "couldn't push excel EndOfStream to output record queue")
		}

		log.Println("excel worker: ErrEndOfStream pushed")
		return execution.ErrEndOfStream
	}

	batch := make([]*execution.Record, 0)
	for i := 0; i < rs.batchSize; i++ {
		record, err := rs.readRecordFromFileWithInitialize()
		if err == execution.ErrEndOfStream {
			break
		} else if err != nil {
			return errors.Wrap(err, "couldn't read record from excel file")
		}

		batch = append(batch, execution.NewRecordFromSlice(
			rs.columnNames,
			record,
			execution.WithID(execution.NewRecordIDFromStreamIDWithOffset(rs.streamID, rs.offset+i))))
	}

	for i := range batch {
		err := outputQueue.Push(ctx, &QueueElement{
			Type: &QueueElement_Record{
				Record: batch[i],
			},
		})
		if err != nil {
			return errors.Wrapf(err, "couldn't push excel record with index %d in batch to output record queue", i)
		}

		log.Println("excel worker: record pushed: ", batch[i])
	}

	rs.offset = rs.offset + len(batch)
	if err := rs.saveOffset(tx); err != nil {
		return errors.Wrap(err, "couldn't save excel offset")
	}

	return nil
}

func (rs *RecordStream) readRecordFromFileWithInitialize() ([]octosql.Value, error) {
	curRow := rs.rows.Columns()

	if rs.first && rs.offset == 0 {
		var cols []octosql.VariableName
		var err error
		if rs.hasHeaderRow {
			cols, err = getHeaderRow(rs.alias, rs.horizontalOffset, curRow)
			if err != nil {
				return nil, errors.Wrap(err, "couldn't get header row")
			}

			if !rs.rows.Next() {
				rs.isDone = true
				return nil, execution.ErrEndOfStream
			}
			curRow = rs.rows.Columns()
		} else {
			cols, err = generatePlaceholders(rs.alias, rs.horizontalOffset, curRow)
			if err != nil {
				return nil, errors.Wrap(err, "couldn't generate placeholder headers")
			}
		}
		rs.first = false

		rs.columnNames = cols

		rs.timeColumns = make([]bool, len(cols))
		for i := range cols {
			if contains(rs.timeColumnNames, cols[i].Name()) {
				rs.timeColumns[i] = true
			}
		}
	}

	row, err := rs.extractRow(curRow)
	if err != nil {
		return nil, errors.Wrap(err, "couldn't extract row")
	}

	if isRowEmpty(row) {
		rs.isDone = true
		return nil, execution.ErrEndOfStream
	}

	if !rs.rows.Next() {
		rs.isDone = true
	}

	return row, nil
}

var offsetPrefix = []byte("excel_offset")

func (rs *RecordStream) loadOffset(tx storage.StateTransaction) error {
	offsetState := storage.NewValueState(tx.WithPrefix(offsetPrefix))

	var offset octosql.Value
	err := offsetState.Get(&offset)
	if err == storage.ErrNotFound {
		offset = octosql.MakeInt(0)
	} else if err != nil {
		return errors.Wrap(err, "couldn't load excel offset from state storage")
	}

	rs.offset = offset.AsInt()

	return nil
}

func (rs *RecordStream) saveOffset(tx storage.StateTransaction) error {
	offsetState := storage.NewValueState(tx.WithPrefix(offsetPrefix))

	offset := octosql.MakeInt(rs.offset)
	err := offsetState.Set(&offset)
	if err != nil {
		return errors.Wrap(err, "couldn't save excel offset to state storage")
	}

	return nil
}

func (rs *RecordStream) Next(ctx context.Context) (*execution.Record, error) {
	tx := storage.GetStateTransactionFromContext(ctx).WithPrefix(rs.streamID.AsPrefix())
	outputQueue := execution.NewOutputQueue(tx.WithPrefix(outputQueuePrefix))

	var queueElement QueueElement
	err := outputQueue.Pop(ctx, &queueElement)
	if err != nil {
		return nil, errors.Wrap(err, "couldn't pop queue element")
	}

	switch queueElement := queueElement.Type.(type) {
	case *QueueElement_Record:
		return queueElement.Record, nil
	case *QueueElement_EndOfStream:
		return nil, execution.ErrEndOfStream
	case *QueueElement_Error:
		return nil, errors.New(queueElement.Error)
	default:
		panic("invalid queue element type")
	}
}

func contains(xs []string, x string) bool {
	for i := range xs {
		if x == xs[i] {
			return true
		}
	}
	return false
}
