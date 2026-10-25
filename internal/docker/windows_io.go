package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/mbianchidev/porto/internal/containerlogs"
	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/mbianchidev/porto/internal/windowscontainers"
)

type windowsIOHub struct {
	config      cio.Config
	input       *io.PipeWriter
	inputReader *io.PipeReader
	mu          sync.Mutex
	subscribers map[uint64]*windowsSubscriber
	next        uint64
	done        chan struct{}
	finished    bool
	history     []runtimes.OutputChunk
	historySize int
	recorder    *containerlogs.Recorder
	closeLog    func() error
	closePipes  func()
	outputsDone <-chan struct{}
	finishOnce  sync.Once
	err         error
}

func memoryWindowsIO(tty, stdin bool, logPath string) (*windowsIOHub, error) {
	reader, writer := io.Pipe()
	hub := &windowsIOHub{input: writer, inputReader: reader, subscribers: make(map[uint64]*windowsSubscriber), done: make(chan struct{})}
	hub.config.Terminal = tty
	if logPath != "" {
		recorder, closeLog, err := containerlogs.Open(logPath)
		if err != nil {
			_ = reader.Close()
			_ = writer.Close()
			return nil, err
		}
		hub.recorder, hub.closeLog = recorder, closeLog
	}
	return hub, nil
}

func newWindowsIOHub(i windowscontainers.Installation, id string, tty, stdin bool, logPath string) (*windowsIOHub, error) {
	hub, err := memoryWindowsIO(tty, stdin, logPath)
	if err != nil {
		return nil, err
	}
	if err := allocateWindowsPipes(i, id, tty, stdin, hub); err != nil {
		return nil, errors.Join(err, hub.Close())
	}
	return hub, nil
}

type windowsStreamWriter struct {
	hub    *windowsIOHub
	stream string
}

func (w windowsStreamWriter) Write(data []byte) (int, error) {
	if w.hub.recorder != nil {
		if _, err := w.hub.recorder.Writer(w.stream).Write(data); err != nil {
			w.hub.mu.Lock()
			w.hub.err = errors.Join(w.hub.err, err)
			w.hub.mu.Unlock()
		}
	}
	w.hub.mu.Lock()
	defer w.hub.mu.Unlock()
	if w.hub.finished {
		return len(data), nil
	}
	chunk := runtimes.OutputChunk{Stream: w.stream, Data: append([]byte(nil), data...)}
	w.hub.history = append(w.hub.history, chunk)
	w.hub.historySize += len(data)
	for len(w.hub.history) > 128 || w.hub.historySize > 1024*1024 {
		w.hub.historySize -= len(w.hub.history[0].Data)
		w.hub.history = w.hub.history[1:]
	}
	for id, subscriber := range w.hub.subscribers {
		chunk := runtimes.OutputChunk{Stream: w.stream, Data: append([]byte(nil), data...)}
		select {
		case subscriber.chunks <- chunk:
		default:
			subscriber.stop(errors.New("native stream subscriber exceeded its bounded output buffer"))
			close(subscriber.chunks)
			delete(w.hub.subscribers, id)
		}
	}
	return len(data), nil
}

type windowsSubscriber struct {
	chunks chan runtimes.OutputChunk
	stop   context.CancelCauseFunc
}

func (h *windowsIOHub) serveWindowsPipe(listener net.Listener, stream string, input bool, closing func() bool, track func(net.Conn) bool) {
	connection, err := listener.Accept()
	if err != nil {
		h.reportTransportFailure("accept "+stream, err, closing())
		return
	}
	if !track(connection) {
		_ = connection.Close()
		return
	}
	defer func() {
		closeErr := connection.Close()
		if !errors.Is(closeErr, net.ErrClosed) && !errors.Is(closeErr, os.ErrClosed) {
			h.reportTransportFailure("close "+stream, closeErr, closing())
		}
	}()
	var copyErr error
	if input {
		_, copyErr = io.Copy(connection, h.inputReader)
	} else {
		_, copyErr = io.Copy(h.writer(stream), connection)
	}
	h.reportTransportFailure("copy "+stream, copyErr, closing())
}

func (h *windowsIOHub) reportTransportFailure(operation string, err error, intentional bool) {
	if err == nil || errors.Is(err, io.EOF) || intentional {
		return
	}
	failure := fmt.Errorf("native runhcs I/O %s: %w", operation, err)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.err = errors.Join(h.err, failure)
	for id, subscriber := range h.subscribers {
		subscriber.stop(failure)
		close(subscriber.chunks)
		delete(h.subscribers, id)
	}
}
func (h *windowsIOHub) writer(stream string) io.Writer { return windowsStreamWriter{h, stream} }
func (h *windowsIOHub) creator() cio.Creator {
	return func(string) (cio.IO, error) { return &windowsCIO{hub: h}, nil }
}

type windowsCIO struct{ hub *windowsIOHub }

func (c *windowsCIO) Config() cio.Config { return c.hub.config }
func (c *windowsCIO) Wait()              {}
func (c *windowsCIO) Cancel()            {}
func (c *windowsCIO) Close() error       { return nil } // the task/exec owner drains and closes the hub

func (h *windowsIOHub) finish() {
	h.finishOnce.Do(func() {
		_ = h.input.Close()
		if h.outputsDone != nil {
			select {
			case <-h.outputsDone:
			case <-time.After(10 * time.Second):
				h.mu.Lock()
				h.err = errors.Join(h.err, errors.New("native runhcs output pipes did not drain before cleanup"))
				h.mu.Unlock()
			}
		}
		if h.closePipes != nil {
			h.closePipes()
		}
		_ = h.inputReader.Close()
		var logErr error
		if h.closeLog != nil {
			logErr = h.closeLog()
		}
		h.mu.Lock()
		h.err = errors.Join(h.err, logErr)
		h.finished = true
		for id, subscriber := range h.subscribers {
			if h.err != nil {
				subscriber.stop(h.err)
			}
			close(subscriber.chunks)
			delete(h.subscribers, id)
		}
		h.mu.Unlock()
		close(h.done)
	})
}
func (h *windowsIOHub) Close() error {
	if h.closePipes != nil {
		h.closePipes()
	}
	h.finish()
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

type windowsSubscription struct {
	output io.ReadCloser
	errors io.ReadCloser
	cancel context.CancelFunc
}

type windowsStreamReader struct {
	*io.PipeReader
	ctx context.Context
}

func (r windowsStreamReader) Read(data []byte) (int, error) {
	n, err := r.PipeReader.Read(data)
	if err != nil {
		if cause := context.Cause(r.ctx); cause != nil {
			return n, cause
		}
	}
	return n, err
}

func (h *windowsIOHub) subscribe(ctx context.Context, replay bool) *windowsSubscription {
	ctx, stop := context.WithCancelCause(ctx)
	cancel := func() { stop(context.Canceled) }
	outReader, outWriter := io.Pipe()
	errReader, errWriter := io.Pipe()
	channel := make(chan runtimes.OutputChunk, 128)
	h.mu.Lock()
	h.next++
	id := h.next
	if replay {
		for _, chunk := range h.history {
			channel <- chunk
		}
	}
	if h.finished {
		close(channel)
	} else {
		h.subscribers[id] = &windowsSubscriber{chunks: channel, stop: stop}
	}
	h.mu.Unlock()
	subscription := &windowsSubscription{output: windowsStreamReader{PipeReader: outReader, ctx: ctx}, errors: windowsStreamReader{PipeReader: errReader, ctx: ctx}, cancel: cancel}
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		defer func() {
			_ = outWriter.CloseWithError(context.Cause(ctx))
			_ = errWriter.CloseWithError(context.Cause(ctx))
		}()
		defer func() { h.mu.Lock(); delete(h.subscribers, id); h.mu.Unlock() }()
		for {
			select {
			case chunk, ok := <-channel:
				if !ok {
					return
				}
				writer := outWriter
				if chunk.Stream == "stderr" {
					writer = errWriter
				}
				if _, err := writer.Write(chunk.Data); err != nil {
					stop(fmt.Errorf("native subscriber output transport: %w", err))
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = outReader.CloseWithError(context.Cause(ctx))
			_ = errReader.CloseWithError(context.Cause(ctx))
		case <-pumpDone:
		}
	}()
	return subscription
}

type windowsSessionProcess struct {
	session      *windowsSession
	subscription *windowsSubscription
	ctx          context.Context
	containerID  string
	exec         bool
	input        bool
	waitOnce     sync.Once
	waitErr      error
}

func (p *windowsSessionProcess) Stdin() io.WriteCloser {
	if !p.input {
		return windowsClosedInput{}
	}
	return &windowsSessionInput{process: p}
}

type windowsClosedInput struct{}

func (windowsClosedInput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func (windowsClosedInput) Close() error              { return nil }

type windowsSessionInput struct{ process *windowsSessionProcess }

func (p *windowsSessionInput) Write(data []byte) (int, error) {
	return p.process.session.hub.input.Write(data)
}
func (p *windowsSessionInput) Close() error {
	if !p.process.exec {
		return nil
	}
	inputErr := p.process.session.hub.input.Close()
	ctx, cancel := context.WithTimeout(windowsContext(context.Background()), 5*time.Second)
	defer cancel()
	return errors.Join(inputErr, p.process.session.process.CloseIO(ctx, containerd.WithStdinCloser))
}
func (p *windowsSessionProcess) Stdout() io.ReadCloser  { return p.subscription.output }
func (p *windowsSessionProcess) Stderr() io.ReadCloser  { return p.subscription.errors }
func (p *windowsSessionProcess) PID() int               { return int(p.session.process.Pid()) }
func (p *windowsSessionProcess) KillOnDisconnect() bool { return p.exec }
func (p *windowsSessionProcess) InputEOF()              { _ = p.Kill() }
func (p *windowsSessionProcess) Kill() error {
	if !p.exec {
		p.subscription.cancel()
		return nil
	}
	ctx, cancel := context.WithTimeout(windowsContext(context.Background()), 10*time.Second)
	defer cancel()
	return killWindowsProcess(ctx, p.session.process)
}
func (p *windowsSessionProcess) Resize(ctx context.Context, w, h uint32) error {
	if w == 0 || h == 0 || w > runtimes.MaxPTYDimension || h > runtimes.MaxPTYDimension {
		return errors.New("invalid terminal dimensions")
	}
	return p.session.process.Resize(windowsContext(ctx), w, h)
}
func (p *windowsSessionProcess) Wait() error {
	p.waitOnce.Do(func() {
		defer p.subscription.cancel()
		select {
		case <-p.session.done:
			p.waitErr = p.session.err
			if p.waitErr == nil && p.session.code != 0 {
				p.waitErr = &containerExitError{code: int(p.session.code)}
			}
		case <-p.ctx.Done():
			if p.exec {
				_ = p.Kill()
				<-p.session.done
			}
			p.waitErr = p.ctx.Err()
		}
	})
	return p.waitErr
}
