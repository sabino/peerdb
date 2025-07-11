package e2e_bigquery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/civil"
	"github.com/shopspring/decimal"
	"google.golang.org/api/iterator"

	peer_bq "github.com/PeerDB-io/peerdb/flow/connectors/bigquery"
	"github.com/PeerDB-io/peerdb/flow/e2eshared"
	"github.com/PeerDB-io/peerdb/flow/generated/protos"
	"github.com/PeerDB-io/peerdb/flow/model"
	"github.com/PeerDB-io/peerdb/flow/shared/types"
)

type BigQueryTestHelper struct {
	// config is the BigQuery config.
	Config *protos.BigqueryConfig
	// client to talk to BigQuery
	client *bigquery.Client
	// runID uniquely identifies the test run to namespace stateful schemas.
	runID uint64
}

// NewBigQueryTestHelper creates a new BigQueryTestHelper.
func NewBigQueryTestHelper(t *testing.T) (*BigQueryTestHelper, error) {
	t.Helper()
	// random 64 bit int to namespace stateful schemas.
	//nolint:gosec // number has no cryptographic significance
	runID := rand.Uint64()

	jsonPath := os.Getenv("TEST_BQ_CREDS")
	if jsonPath == "" {
		return nil, errors.New("TEST_BQ_CREDS env var not set")
	}

	content, err := e2eshared.ReadFileToBytes(jsonPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	var config *protos.BigqueryConfig
	if err := json.Unmarshal(content, &config); err != nil {
		return nil, fmt.Errorf("failed to unmarshal json: %w", err)
	}

	// suffix the dataset with the runID to namespace stateful schemas.
	config.DatasetId = fmt.Sprintf("%s_%d", config.DatasetId, runID)

	bqsa, err := peer_bq.NewBigQueryServiceAccount(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create BigQueryServiceAccount: %v", err)
	}

	client, err := bqsa.CreateBigQueryClient(t.Context())
	if err != nil {
		return nil, fmt.Errorf("failed to create helper BigQuery client: %v", err)
	}

	return &BigQueryTestHelper{
		runID:  runID,
		Config: config,
		client: client,
	}, nil
}

// datasetExists checks if the dataset exists.
func (b *BigQueryTestHelper) datasetExists(ctx context.Context, dataset *bigquery.Dataset) (bool, error) {
	meta, err := dataset.Metadata(ctx)
	if err != nil {
		// if err message contains `notFound` then dataset does not exist.
		if strings.Contains(err.Error(), "notFound") {
			return false, nil
		}

		return false, fmt.Errorf("failed to get dataset metadata: %w", err)
	}

	if meta == nil {
		return false, nil
	}

	return true, nil
}

// RecreateDataset recreates the dataset, i.e, deletes it if exists and creates it again.
func (b *BigQueryTestHelper) RecreateDataset(ctx context.Context) error {
	dataset := b.client.Dataset(b.Config.DatasetId)

	exists, err := b.datasetExists(ctx, dataset)
	if err != nil {
		return fmt.Errorf("failed to check if dataset %s exists: %w", b.Config.DatasetId, err)
	}

	if exists {
		if err := dataset.DeleteWithContents(ctx); err != nil {
			return fmt.Errorf("failed to delete dataset: %w", err)
		}
	}

	if err := dataset.Create(ctx, &bigquery.DatasetMetadata{
		DefaultTableExpiration:     time.Hour,
		DefaultPartitionExpiration: time.Hour,
	}); err != nil {
		return fmt.Errorf("failed to create dataset: %w", err)
	}

	return nil
}

// DropDataset drops the dataset.
func (b *BigQueryTestHelper) DropDataset(ctx context.Context, datasetName string) error {
	dataset := b.client.Dataset(datasetName)
	exists, err := b.datasetExists(ctx, dataset)
	if err != nil {
		return fmt.Errorf("failed to check if dataset %s exists: %w", b.Config.DatasetId, err)
	}

	if exists {
		if err := dataset.DeleteWithContents(ctx); err != nil {
			return fmt.Errorf("failed to delete dataset: %w", err)
		}
	}

	return nil
}

// countRows(tableName) returns the number of rows in the given table.
func (b *BigQueryTestHelper) countRows(ctx context.Context, tableName string) (int, error) {
	return b.countRowsWithDataset(ctx, b.Config.DatasetId, tableName, "")
}

func (b *BigQueryTestHelper) countRowsWithDataset(ctx context.Context, dataset, tableName string, nonNullCol string) (int, error) {
	command := fmt.Sprintf("SELECT COUNT(*) FROM `%s.%s`", dataset, tableName)
	if nonNullCol != "" {
		command = "SELECT COUNT(CASE WHEN " + nonNullCol +
			" IS NOT NULL THEN 1 END) AS non_null_count FROM `" + dataset + "." + tableName + "`;"
	}
	q := b.client.Query(command)
	q.DisableQueryCache = true
	it, err := q.Read(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to run command: %w", err)
	}

	var row []bigquery.Value
	for {
		err := it.Next(&row)
		if err == iterator.Done {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("failed to iterate over query results: %w", err)
		}
	}

	cntI64, ok := row[0].(int64)
	if !ok {
		return 0, errors.New("failed to convert row count to int64")
	}

	return int(cntI64), nil
}

func toQValue(bqValue bigquery.Value) (types.QValue, error) {
	// Based on the real type of the bigquery.Value, we create a types.QValue
	switch v := bqValue.(type) {
	case int64:
		return types.QValueInt64{Val: v}, nil
	case float64:
		return types.QValueFloat64{Val: v}, nil
	case string:
		return types.QValueString{Val: v}, nil
	case bool:
		return types.QValueBoolean{Val: v}, nil
	case civil.Date:
		return types.QValueDate{Val: v.In(time.UTC)}, nil
	case civil.Time:
		return types.QValueTime{Val: time.Duration(v.Hour)*time.Hour +
			time.Duration(v.Minute)*time.Minute +
			time.Duration(v.Second)*time.Second +
			time.Duration(v.Nanosecond)*time.Nanosecond}, nil
	case time.Time:
		return types.QValueTimestamp{Val: v}, nil
	case *big.Rat:
		return types.QValueNumeric{Val: decimal.NewFromBigRat(v, 32)}, nil
	case []uint8:
		return types.QValueBytes{Val: v}, nil
	case []bigquery.Value:
		// If the type is an array, we need to convert each element
		// we can assume all elements are of the same type, let us use first element
		if len(v) == 0 {
			return types.QValueNull(types.QValueKindInvalid), nil
		}

		firstElement := v[0]
		switch et := firstElement.(type) {
		case int64:
			var arr []int64
			for _, val := range v {
				arr = append(arr, val.(int64))
			}
			return types.QValueArrayInt64{Val: arr}, nil
		case float64:
			var arr []float64
			for _, val := range v {
				arr = append(arr, val.(float64))
			}
			return types.QValueArrayFloat64{Val: arr}, nil
		case string:
			var arr []string
			for _, val := range v {
				arr = append(arr, val.(string))
			}
			return types.QValueArrayString{Val: arr}, nil
		case time.Time:
			var arr []time.Time
			for _, val := range v {
				arr = append(arr, val.(time.Time))
			}
			return types.QValueArrayTimestamp{Val: arr}, nil
		case civil.Date:
			var arr []time.Time
			for _, val := range v {
				arr = append(arr, val.(civil.Date).In(time.UTC))
			}
			return types.QValueArrayDate{Val: arr}, nil
		case bool:
			var arr []bool
			for _, val := range v {
				arr = append(arr, val.(bool))
			}
			return types.QValueArrayBoolean{Val: arr}, nil
		case *big.Rat:
			var arr []decimal.Decimal
			for _, val := range v {
				arr = append(arr, decimal.NewFromBigRat(val.(*big.Rat), 32))
			}
			return types.QValueArrayNumeric{Val: arr}, nil
		default:
			// If type is unsupported, return error
			return nil, fmt.Errorf("bqHelper unsupported type %T", et)
		}

	case nil:
		return types.QValueNull(types.QValueKindInvalid), nil
	default:
		// If type is unsupported, return error
		return nil, fmt.Errorf("bqHelper unsupported type %T", v)
	}
}

// bqSchemaToQRecordSchema converts a bigquery schema to a QRecordSchema.
func bqSchemaToQRecordSchema(schema bigquery.Schema) types.QRecordSchema {
	fields := make([]types.QField, 0, len(schema))
	for _, fieldSchema := range schema {
		fields = append(fields, peer_bq.BigQueryFieldToQField(fieldSchema))
	}
	return types.QRecordSchema{Fields: fields}
}

func (b *BigQueryTestHelper) ExecuteAndProcessQuery(ctx context.Context, query string) (*model.QRecordBatch, error) {
	q := b.client.Query(query)
	q.DisableQueryCache = true
	it, err := q.Read(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to run command: %w", err)
	}

	var records [][]types.QValue
	for {
		var row []bigquery.Value
		if err := it.Next(&row); err != nil {
			if err == iterator.Done {
				break
			}
			return nil, fmt.Errorf("failed to iterate over query results: %w", err)
		}

		// Convert []bigquery.Value to []types.QValue
		qValues := make([]types.QValue, len(row))
		for i, val := range row {
			qv, err := toQValue(val)
			if err != nil {
				return nil, err
			}
			qValues[i] = qv
		}

		records = append(records, qValues)
	}

	// Now fill column names as well. Assume the schema is retrieved from the query itself
	schema := bqSchemaToQRecordSchema(it.Schema)

	return &model.QRecordBatch{
		Schema:  schema,
		Records: records,
	}, nil
}

// returns whether the function errors or there are no nulls
func (b *BigQueryTestHelper) CheckNull(ctx context.Context, tableName string, colName []string) (bool, error) {
	if len(colName) == 0 {
		return true, nil
	}
	joinedString := strings.Join(colName, " is null or ") + " is null"
	command := fmt.Sprintf("SELECT COUNT(*) FROM `%s.%s` WHERE %s",
		b.Config.DatasetId, tableName, joinedString)
	q := b.client.Query(command)
	q.DisableQueryCache = true
	it, err := q.Read(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to run command: %w", err)
	}

	var row []bigquery.Value
	for {
		err := it.Next(&row)
		if err == iterator.Done {
			break
		}
		if err != nil {
			return false, fmt.Errorf("failed to iterate over query results: %w", err)
		}
	}

	cntI64, ok := row[0].(int64)
	if !ok {
		return false, errors.New("failed to convert row count to int64")
	}
	if cntI64 > 0 {
		return false, nil
	} else {
		return true, nil
	}
}

// check if NaN, Inf double values are null
func (b *BigQueryTestHelper) SelectRow(ctx context.Context, tableName string, cols ...string) ([]bigquery.Value, error) {
	command := fmt.Sprintf("SELECT %s FROM `%s.%s`",
		strings.Join(cols, ","), b.Config.DatasetId, tableName)
	q := b.client.Query(command)
	q.DisableQueryCache = true
	it, err := q.Read(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to run command: %w", err)
	}

	var row []bigquery.Value
	for {
		err := it.Next(&row)
		if err == iterator.Done {
			return row, nil
		}
		if err != nil {
			return nil, fmt.Errorf("failed to iterate over query results: %w", err)
		}
	}
}

func (b *BigQueryTestHelper) RunInt64Query(ctx context.Context, query string) (int64, error) {
	recordBatch, err := b.ExecuteAndProcessQuery(ctx, query)
	if err != nil {
		return 0, fmt.Errorf("could not execute query: %w", err)
	}
	if len(recordBatch.Records) != 1 {
		return 0, fmt.Errorf("expected only 1 record, got %d", len(recordBatch.Records))
	}

	if v, ok := recordBatch.Records[0][0].(types.QValueInt64); ok {
		return v.Val, nil
	}
	return 0, fmt.Errorf("non-integer result: %T", recordBatch.Records[0][0])
}
