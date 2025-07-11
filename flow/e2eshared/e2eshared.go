package e2eshared

import (
	"context"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/PeerDB-io/peerdb/flow/model"
	"github.com/PeerDB-io/peerdb/flow/model/qvalue"
	"github.com/PeerDB-io/peerdb/flow/shared/types"
)

type Suite interface {
	Teardown(context.Context)
}

func RunSuite[T Suite](t *testing.T, setup func(t *testing.T) T) {
	t.Helper()
	t.Parallel()

	typ := reflect.TypeFor[T]()
	mcount := typ.NumMethod()
	for i := range mcount {
		m := typ.Method(i)
		if strings.HasPrefix(m.Name, "Test") {
			if m.Type.NumIn() == 1 && m.Type.NumOut() == 0 {
				t.Run(m.Name, func(subtest *testing.T) {
					subtest.Parallel()
					suite := setup(subtest)
					subtest.Cleanup(func() {
						ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
						defer cancel()
						suite.Teardown(ctx)
					})
					m.Func.Call([]reflect.Value{reflect.ValueOf(suite)})
				})
			}
		}
	}
}

func RunSuiteNoParallel[T Suite](t *testing.T, setup func(t *testing.T) T) {
	t.Helper()

	typ := reflect.TypeFor[T]()
	mcount := typ.NumMethod()
	for i := range mcount {
		m := typ.Method(i)
		if strings.HasPrefix(m.Name, "Test") {
			if m.Type.NumIn() == 1 && m.Type.NumOut() == 0 {
				t.Run(m.Name, func(subtest *testing.T) {
					suite := setup(subtest)
					subtest.Cleanup(func() {
						ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
						defer cancel()
						suite.Teardown(ctx)
					})
					m.Func.Call([]reflect.Value{reflect.ValueOf(suite)})
				})
			}
		}
	}
}

func ReadFileToBytes(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	defer f.Close()

	return io.ReadAll(f)
}

// checks if two QRecords are identical
func CheckQRecordEquality(t *testing.T, q []types.QValue, other []types.QValue) bool {
	t.Helper()

	if len(q) != len(other) {
		t.Logf("unequal entry count: %d != %d", len(q), len(other))
		return false
	}

	maybeTruncate := func(v types.QValue) string {
		s := fmt.Sprintf("%+v", v)
		// truncate log for extremely large documents
		if len(s) > 1_000_000 {
			return s[:100] + "...[truncated]"
		}
		return s
	}

	for i, entry := range q {
		otherEntry := other[i]
		if !qvalue.Equals(entry, otherEntry) {
			t.Logf("entry %d: %T %+v != %T %+v", i, entry, maybeTruncate(entry), otherEntry, maybeTruncate(otherEntry))
			return false
		}
	}

	return true
}

// Equals checks if two QRecordBatches are identical.
func CheckEqualRecordBatches(t *testing.T, q *model.QRecordBatch, other *model.QRecordBatch) bool {
	t.Helper()

	if q == nil || other == nil {
		t.Logf("q nil? %v, other nil? %v", q == nil, other == nil)
		return q == nil && other == nil
	}

	// First check simple attributes
	if len(q.Records) != len(other.Records) {
		// print num records
		t.Logf("q.NumRecords: %d", len(q.Records))
		t.Logf("other.NumRecords: %d", len(other.Records))
		return false
	}

	// Compare column names
	if !q.Schema.EqualNames(other.Schema) {
		t.Log("Column names are not equal")
		t.Logf("Schema 1: %v", q.Schema.GetColumnNames())
		t.Logf("Schema 2: %v", other.Schema.GetColumnNames())
		return false
	}

	// Compare records
	for i, record := range q.Records {
		if !CheckQRecordEquality(t, record, other.Records[i]) {
			t.Logf("Record %d is not equal", i)
			return false
		}
	}

	return true
}
