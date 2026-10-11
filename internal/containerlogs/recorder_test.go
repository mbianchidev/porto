package containerlogs

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRecorderPreservesRawAndSeparatesBoundedStreams(t *testing.T) {
	var raw, journal bytes.Buffer
	recorder := New(&raw, &journal)
	if _, err := recorder.Writer("stdout").Write([]byte("first\npartial")); err != nil {
		t.Fatal(err)
	}
	if _, err := recorder.Writer("stderr").Write([]byte("error\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := recorder.Writer("stdout").Write([]byte(strings.Repeat("x", MaxLineBytes*2))); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Flush(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw.String(), "first\npartialerror\n") {
		t.Fatalf("raw output changed: %.40s", raw.String())
	}
	decoder := json.NewDecoder(&journal)
	var records []Record
	for decoder.More() {
		var record Record
		if err := decoder.Decode(&record); err != nil {
			t.Fatal(err)
		}
		if record.Timestamp.IsZero() || len(record.Text) > MaxLineBytes {
			t.Fatalf("unbounded or untimed record: %+v", record)
		}
		records = append(records, record)
	}
	if len(records) < 4 || records[0].Stream != "stdout" || records[1].Stream != "stderr" {
		t.Fatalf("stream metadata was lost: %+v", records)
	}
}
