package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"time"

	containersapi "github.com/containerd/containerd/api/services/containers/v1"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/mbianchidev/porto/internal/containerlogs"
	"github.com/mbianchidev/porto/internal/runtimes"
)

const (
	portoLogURILabel     = "io.porto.container.log-uri"
	portoLogJournalLabel = "io.porto.container.log-journal"
)

type LogCapabilities struct {
	Timestamps bool   `json:"timestamps"`
	Streams    bool   `json:"streams"`
	Message    string `json:"message,omitempty"`
}

func (r *grpcContainerRuntime) directLogURI(ctx context.Context, logPath string) (*url.URL, error) {
	if r.helperPath == "" {
		return &url.URL{Scheme: "file", Path: filepath.ToSlash(logPath)}, nil
	}
	helper := r.helperPath
	if r.lima != "" {
		output, err := r.runRuntimeHelper(ctx, "path")
		if err != nil {
			return nil, err
		}
		helper = strings.TrimSpace(string(output))
		if !path.IsAbs(helper) {
			return nil, errors.New("runtime helper reported an invalid logger path")
		}
	}
	return cio.LogURIGenerator("binary", helper, map[string]string{"porto-log": logPath})
}

func logURIForLabels(labels map[string]string) string {
	if uri := labels[portoLogURILabel]; uri != "" {
		return uri
	}
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(labels[portoLogPathLabel])}).String()
}

func (m *Manager) StreamInspectorLogs(ctx context.Context, id string, tail int, follow bool, metadata func(LogCapabilities) error, emit func(containerlogs.Record) error) (err error) {
	if tail < 0 || tail > 5000 {
		return fmt.Errorf("%w: log tail must be between 0 and 5000", ErrUnsupported)
	}
	client, err := m.runtimeConnector(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, client.Close()) }()
	backend, ok := client.(*grpcContainerRuntime)
	if !ok {
		return fmt.Errorf("%w: inspector log capabilities could not be resolved", ErrUnsupported)
	}
	id, err = backend.resolveContainerID(ctx, id)
	if err != nil {
		return err
	}
	record, err := backend.containers.Get(withContainerdNamespace(ctx, backend.namespace), &containersapi.GetContainerRequest{ID: id})
	if err != nil {
		return err
	}
	journal := record.GetContainer().GetLabels()[portoLogJournalLabel]
	capabilities := LogCapabilities{Timestamps: journal != "", Streams: journal != ""}
	if journal == "" {
		capabilities.Message = "This older container has combined, untimed raw logs. New containers created with the packaged runtime helper retain stdout/stderr and capture timestamps; existing history is not rewritten."
	}
	if err := metadata(capabilities); err != nil {
		return err
	}
	if journal == "" {
		var pending []byte
		streamErr := m.StreamDockerContainerLogs(ctx, id, LogOptions{
			Stdout: true, Stderr: true, Tail: fmt.Sprint(tail), Follow: follow,
		}, func(chunk runtimes.OutputChunk) error {
			pending = append(pending, chunk.Data...)
			for len(pending) > 0 {
				take := min(len(pending), containerlogs.MaxLineBytes)
				if newline := bytes.IndexByte(pending[:take], '\n'); newline >= 0 {
					take = newline + 1
				} else if len(pending) < containerlogs.MaxLineBytes {
					break
				}
				if err := emit(containerlogs.Record{ReceivedAt: time.Now().UTC(), Stream: "combined", Text: strings.ToValidUTF8(string(pending[:take]), "\uFFFD")}); err != nil {
					return err
				}
				pending = pending[take:]
			}
			return nil
		})
		if streamErr == nil && len(pending) > 0 {
			streamErr = emit(containerlogs.Record{ReceivedAt: time.Now().UTC(), Stream: "combined", Text: strings.ToValidUTF8(string(pending), "\uFFFD"), Partial: true})
		}
		return streamErr
	}
	return backend.streamJournal(ctx, journal, tail, follow, emit)
}

func (backend *grpcContainerRuntime) streamJournal(ctx context.Context, journal string, tail int, follow bool, emit func(containerlogs.Record) error) error {
	var pending []byte
	parse := func(chunk runtimes.OutputChunk) error {
		if chunk.Stream == "stderr" {
			return fmt.Errorf("container log transport reported an error: %.1024s", chunk.Data)
		}
		pending = append(pending, chunk.Data...)
		for {
			newline := bytes.IndexByte(pending, '\n')
			if newline < 0 {
				if len(pending) > containerlogs.MaxLineBytes*8 {
					return errors.New("container journal record exceeds its bounded size")
				}
				return nil
			}
			var record containerlogs.Record
			if err := json.Unmarshal(pending[:newline], &record); err != nil {
				return fmt.Errorf("decode container log journal: %w", err)
			}
			if record.Stream != "stdout" && record.Stream != "stderr" || len(record.Text) > containerlogs.MaxLineBytes {
				return errors.New("invalid bounded container journal record")
			}
			if err := emit(record); err != nil {
				return err
			}
			pending = pending[newline+1:]
		}
	}
	if backend.lima != "" {
		return backend.streamLimaLog(ctx, journal, tail, follow, parse)
	}
	return streamLocalLog(ctx, journal, tail, follow, parse)
}
