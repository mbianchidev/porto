package containerlogs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const MaxLineBytes = 32 * 1024

type Record struct {
	Timestamp  time.Time `json:"timestamp,omitzero"`
	ReceivedAt time.Time `json:"receivedAt,omitzero"`
	Stream     string    `json:"stream"`
	Text       string    `json:"text"`
	Partial    bool      `json:"partial,omitempty"`
}

type Recorder struct {
	mu      sync.Mutex
	raw     io.Writer
	journal io.Writer
	pending map[string][]byte
}

func New(raw, journal io.Writer) *Recorder {
	return &Recorder{raw: raw, journal: journal, pending: make(map[string][]byte)}
}

func Open(logPath string) (*Recorder, func() error, error) {
	if !filepath.IsAbs(logPath) || strings.ContainsAny(logPath, "\r\n\x00") {
		return nil, nil, errors.New("invalid container log path")
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return nil, nil, err
	}
	raw, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, err
	}
	journal, err := os.OpenFile(logPath+".jsonl", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, errors.Join(err, raw.Close())
	}
	recorder := New(raw, journal)
	var once sync.Once
	var closeErr error
	return recorder, func() error {
		once.Do(func() { closeErr = errors.Join(recorder.Flush(), raw.Close(), journal.Close()) })
		return closeErr
	}, nil
}

type streamWriter struct {
	recorder *Recorder
	stream   string
}

func (r *Recorder) Writer(stream string) io.Writer {
	return streamWriter{recorder: r, stream: stream}
}

func (w streamWriter) Write(data []byte) (int, error) {
	if w.stream != "stdout" && w.stream != "stderr" {
		return 0, errors.New("invalid container log stream")
	}
	r := w.recorder
	r.mu.Lock()
	defer r.mu.Unlock()
	n, err := r.raw.Write(data)
	if err != nil {
		return n, err
	}
	remaining := data[:n]
	for len(remaining) > 0 {
		buffer := r.pending[w.stream]
		take := min(MaxLineBytes-len(buffer), len(remaining))
		newline := bytes.IndexByte(remaining[:take], '\n')
		complete := newline >= 0
		if complete {
			take = newline + 1
		}
		buffer = append(buffer, remaining[:take]...)
		remaining = remaining[take:]
		if complete || len(buffer) == MaxLineBytes {
			if err := r.emit(w.stream, buffer, !complete); err != nil {
				return n, err
			}
			buffer = nil
		}
		r.pending[w.stream] = buffer
	}
	return n, nil
}

func (r *Recorder) Flush() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, stream := range []string{"stdout", "stderr"} {
		if buffer := r.pending[stream]; len(buffer) > 0 {
			if err := r.emit(stream, buffer, true); err != nil {
				return err
			}
			r.pending[stream] = nil
		}
	}
	return nil
}

func (r *Recorder) emit(stream string, data []byte, partial bool) error {
	text := strings.ToValidUTF8(string(data), "\uFFFD")
	if len(text) > MaxLineBytes {
		text = text[:MaxLineBytes]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
	}
	record := Record{Timestamp: time.Now().UTC(), Stream: stream, Text: text, Partial: partial}
	document, err := json.Marshal(record)
	if err != nil {
		return err
	}
	document = append(document, '\n')
	if _, err := r.journal.Write(document); err != nil {
		return fmt.Errorf("write timestamped container log: %w", err)
	}
	return nil
}
