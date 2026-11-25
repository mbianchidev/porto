package docker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"time"

	"github.com/porto-containers/porto/internal/runtimes"
)

// startLogAttach attaches to a non-terminal compatibility container. nerdctl's attach cannot open
// output that goes to the log driver and would compete for FIFOs that another start still reads,
// so output follows the container's logs from now until it exits, and input goes to its stdin FIFO.
func (m *Manager) startLogAttach(ctx context.Context, details containerAttachDetails, stdin bool) (runtimes.Process, error) {
	var forwarder compatibilityInputForwarder
	closeForwarder := func() error { return nil }
	if stdin && details.OpenStdin {
		var err error
		forwarder, closeForwarder, err = m.taskInputForwarder(ctx)
		if err != nil {
			return nil, err
		}
		fifo, err := forwarder.TaskInput(ctx, details.ID)
		if err == nil && fifo == "" {
			err = errTaskInputClosed
		}
		if err != nil {
			return nil, errors.Join(err, closeForwarder())
		}
	}
	process, err := m.startProcess(ctx, "follow Porto container output", "logs", "--follow", "--since", time.Now().UTC().Format(time.RFC3339Nano), details.ID)
	if err != nil {
		return nil, errors.Join(err, closeForwarder())
	}
	if forwarder == nil {
		return process, nil
	}
	return wrapTaskInput(ctx, details.ID, process, forwarder, closeForwarder, taskInputOptions{closeOnEOF: details.StdinOnce}), nil
}

// nerdctl 2.4 tees a foreground container's output into its logging binary and
// warns on its own stderr when that binary exits before a short-lived container
// finishes writing. Docker never mixes runtime diagnostics into container stderr.
const (
	nerdctlLogLinePrefix        = `time="`
	nerdctlLoggingBinaryWarning = ` level=warning msg="writing container output to the logging binary failed; further output will not be logged"`
	maxHeldNerdctlLogLine       = 4096
)

type nerdctlAttachProcess struct {
	runtimes.Process
	stderr io.ReadCloser
}

func (p *nerdctlAttachProcess) Stderr() io.ReadCloser {
	return p.stderr
}

type filteredStderr struct {
	*io.PipeReader
	source io.Closer
}

func (s filteredStderr) Close() error {
	return errors.Join(s.PipeReader.Close(), s.source.Close())
}

func withoutNerdctlLoggingWarnings(process runtimes.Process) runtimes.Process {
	source := process.Stderr()
	reader, writer := io.Pipe()
	go func() {
		err := copyWithoutNerdctlLoggingWarnings(writer, source)
		if err != nil {
			_, _ = io.Copy(io.Discard, source)
		}
		_ = writer.CloseWithError(err)
	}()
	return &nerdctlAttachProcess{Process: process, stderr: filteredStderr{PipeReader: reader, source: source}}
}

// copyWithoutNerdctlLoggingWarnings holds back only a line that starts like a
// nerdctl log entry until its newline, so prompts and partial lines still
// stream immediately.
func copyWithoutNerdctlLoggingWarnings(destination io.Writer, source io.Reader) error {
	buffer := make([]byte, 32*1024)
	var held []byte
	passThrough := false
	for {
		count, readErr := source.Read(buffer)
		chunk := buffer[:count]
		for len(chunk) > 0 {
			end := bytes.IndexByte(chunk, '\n') + 1
			complete := end > 0
			if !complete {
				end = len(chunk)
			}
			line := chunk[:end]
			chunk = chunk[end:]
			if passThrough {
				passThrough = !complete
				if _, err := destination.Write(line); err != nil {
					return err
				}
				continue
			}
			held = append(held, line...)
			prefix := min(len(held), len(nerdctlLogLinePrefix))
			if string(held[:prefix]) == nerdctlLogLinePrefix[:prefix] && !complete && len(held) <= maxHeldNerdctlLogLine {
				continue
			}
			if !complete || !bytes.Contains(held, []byte(nerdctlLoggingBinaryWarning)) {
				if _, err := destination.Write(held); err != nil {
					return err
				}
			}
			passThrough = !complete
			held = held[:0]
		}
		if readErr != nil {
			if len(held) > 0 {
				if _, err := destination.Write(held); err != nil {
					return err
				}
			}
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}
