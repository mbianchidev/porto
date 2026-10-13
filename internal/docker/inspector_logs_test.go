package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/containerlogs"
	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestInspectorJournalPreservesStreamsAndCancellation(t *testing.T) {
	directory := t.TempDir()
	journal := filepath.Join(directory, "fixture.jsonl")
	var document strings.Builder
	for _, stream := range []string{"stdout", "stderr"} {
		record, err := json.Marshal(containerlogs.Record{
			Timestamp: time.Unix(1, 0).UTC(), Stream: stream, Text: "synthetic\n",
		})
		if err != nil {
			t.Fatal(err)
		}
		document.Write(record)
		document.WriteByte('\n')
	}
	if err := os.WriteFile(journal, []byte(document.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	backend := &grpcContainerRuntime{}
	var records []containerlogs.Record
	emit := func(record containerlogs.Record) error {
		records = append(records, record)
		return nil
	}
	if err := backend.streamJournal(context.Background(), journal, 500, false, emit); err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].Stream != "stdout" || records[1].Stream != "stderr" ||
		!records[0].Timestamp.Equal(time.Unix(1, 0)) {
		t.Fatalf("invalid retained stream records: %+v", records)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := backend.streamJournal(ctx, journal, 1, true, func(record containerlogs.Record) error {
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("stream did not preserve cancellation: %v", err)
	}
}

func TestInspectorJournalRejectsInvalidAndIncompleteRecords(t *testing.T) {
	for _, document := range []string{
		"{invalid}\n",
		"{\"stream\":\"other\",\"text\":\"synthetic\"}\n",
		"{\"stream\":\"stdout\",\"text\":\"synthetic\"}",
		"{\"stream\":\"stdout\",\"text\":\"" + strings.Repeat("x", containerlogs.MaxLineBytes+1) + "\"}\n",
		strings.Repeat("x", containerlogs.MaxLineBytes*8+1),
	} {
		journal := filepath.Join(t.TempDir(), "fixture.jsonl")
		if err := os.WriteFile(journal, []byte(document), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := (&grpcContainerRuntime{}).streamJournal(context.Background(), journal, 500, false,
			func(containerlogs.Record) error { return nil }); err == nil {
			t.Fatal("invalid or incomplete journal silently succeeded")
		}
	}
}

func TestInspectorLogTransportBoundsChunksAndStopsPromptly(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "fixture.log")
	if err := os.WriteFile(journal, []byte(strings.Repeat("x", 1024*1024)), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var chunks, maximum int
	err := streamLocalLog(ctx, journal, 500, true, func(chunk runtimes.OutputChunk) error {
		chunks++
		maximum = max(maximum, len(chunk.Data))
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || maximum > 4096 || chunks != 1 {
		t.Fatalf("unbounded or uncancellable transport: chunks=%d, bytes=%d, error=%v", chunks, maximum, err)
	}
}

func TestInspectorLogTransportPreservesChunkOwnership(t *testing.T) {
	expected := strings.Repeat("a", 4096) + "\n" + strings.Repeat("b", 4096) + "\n"
	journal := filepath.Join(t.TempDir(), "fixture.log")
	if err := os.WriteFile(journal, []byte(expected), 0o600); err != nil {
		t.Fatal(err)
	}
	var chunks [][]byte
	if err := streamLocalLog(context.Background(), journal, 500, false, func(chunk runtimes.OutputChunk) error {
		chunks = append(chunks, chunk.Data)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if actual := bytes.Join(chunks, nil); string(actual) != expected {
		t.Fatal("a subsequent read changed retained log chunks")
	}
}
