package docker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"

	"github.com/mbianchidev/porto/internal/runtimes"
)

func (a *API) attachContainerLogs(w http.ResponseWriter, r *http.Request) {
	details, err := a.manager.containerAttachDetails(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDockerError(w, err)
		return
	}
	ctx, cancel := context.WithCancel(dockerServerContext(r.Context()))
	defer cancel()
	follow := dockerBool(r, "stream") && details.State.active()
	var input runtimes.Process
	if follow && dockerBool(r, "stdin") {
		input, err = a.manager.StartAttach(ctx, details.ID, true, details.terminal()...)
		if err != nil {
			writeDockerError(w, err)
			return
		}
		defer func() {
			_ = input.Stdin().Close()
			_ = input.Kill()
			_ = input.Wait()
		}()
	}
	connection, err := hijackDockerStream(w, r)
	if err != nil {
		return
	}
	defer connection.Close()
	if input != nil {
		go func() {
			_, _ = io.Copy(input.Stdin(), connection)
			_ = input.Stdin().Close()
		}()
		go func() { _, _ = io.Copy(io.Discard, input.Stdout()) }()
		go func() { _, _ = io.Copy(io.Discard, input.Stderr()) }()
	}
	options := LogOptions{
		Stdout: dockerBool(r, "stdout"), Stderr: dockerBool(r, "stderr"),
		Tail: "all", Follow: follow,
	}
	var mu sync.Mutex
	delivered := make(map[string]int64)
	emit := func(chunk runtimes.OutputChunk) error {
		stream := byte(1)
		if chunk.Stream == "stderr" {
			stream = 2
		}
		writer := &dockerProcessWriter{connection: connection, stream: stream, tty: details.TTY, mu: &mu}
		_, err := writer.Write(chunk.Data)
		if err == nil {
			delivered[chunk.Stream] += int64(len(chunk.Data))
		}
		return err
	}
	if !follow {
		if err := a.manager.StreamDockerContainerLogs(ctx, details.ID, options, emit); err != nil {
			writeDockerStreamError(connection, details.TTY, err)
		}
		return
	}
	logContext, stopLogs := context.WithCancel(ctx)
	defer stopLogs()
	finished := make(chan error, 1)
	go func() {
		_, err := a.manager.WaitContainer(logContext, details.ID, "not-running")
		finished <- err
		stopLogs()
	}()
	streamErr := a.manager.StreamDockerContainerLogs(logContext, details.ID, options, emit)
	stopLogs()
	waitErr := <-finished
	if ctx.Err() != nil {
		return
	}
	if waitErr != nil {
		if !errors.Is(waitErr, context.Canceled) {
			writeDockerStreamError(connection, details.TTY, waitErr)
		} else if streamErr != nil && !errors.Is(streamErr, context.Canceled) {
			writeDockerStreamError(connection, details.TTY, streamErr)
		}
		return
	}
	if streamErr != nil && !errors.Is(streamErr, context.Canceled) {
		writeDockerStreamError(connection, details.TTY, streamErr)
		return
	}
	// Read the final journal suffix after task exit without replaying delivered bytes.
	options.Follow = false
	skip := delivered
	err = a.manager.StreamDockerContainerLogs(ctx, details.ID, options, func(chunk runtimes.OutputChunk) error {
		count := min(skip[chunk.Stream], int64(len(chunk.Data)))
		skip[chunk.Stream] -= count
		chunk.Data = chunk.Data[count:]
		if len(chunk.Data) == 0 {
			return nil
		}
		stream := byte(1)
		if chunk.Stream == "stderr" {
			stream = 2
		}
		writer := &dockerProcessWriter{connection: connection, stream: stream, tty: details.TTY, mu: &mu}
		_, err := writer.Write(chunk.Data)
		return err
	})
	if err != nil {
		writeDockerStreamError(connection, details.TTY, err)
	}
}
