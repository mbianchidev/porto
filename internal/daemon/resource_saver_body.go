package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/mbianchidev/porto/internal/resourcesaver"
)

type queuedRequestBody struct{ *os.File }

func (body *queuedRequestBody) Close() error {
	closeErr := body.File.Close()
	if errors.Is(closeErr, os.ErrClosed) {
		closeErr = nil
	}
	return errors.Join(closeErr, os.Remove(body.Name()))
}

func stageQueuedRequestBody(ctx context.Context, w http.ResponseWriter, request *http.Request, limit int64) (_ *queuedRequestBody, err error) {
	if request.Body == nil || request.Body == http.NoBody {
		return nil, nil
	}
	controller := http.NewResponseController(w)
	if request.ProtoMajor == 1 {
		if err := controller.EnableFullDuplex(); err != nil {
			return nil, fmt.Errorf("prepare queued HTTP upload: %w", err)
		}
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, errors.New("queued HTTP upload requires a bounded deadline")
	}
	if err := controller.SetReadDeadline(deadline); err != nil {
		return nil, fmt.Errorf("bound queued HTTP upload: %w", err)
	}
	defer func() {
		if resetErr := controller.SetReadDeadline(time.Time{}); resetErr != nil {
			slog.Warn("Reset Resource Saver upload deadline", "error", resetErr)
			err = errors.Join(err, resetErr)
		}
	}()
	file, err := os.CreateTemp("", "porto-queued-body-")
	if err != nil {
		return nil, fmt.Errorf("stage bounded queued body: %w", err)
	}
	body := &queuedRequestBody{File: file}
	defer func() {
		if err != nil {
			err = errors.Join(err, body.Close())
		}
	}()
	// Reading the bounded body enables net/http's disconnect monitoring during
	// readiness wait; leaving it unread hides client cancellation on HTTP/1.
	_, copyErr := io.Copy(file, http.MaxBytesReader(w, request.Body, limit))
	closeErr := request.Body.Close()
	if copyErr != nil || closeErr != nil {
		var tooLarge *http.MaxBytesError
		var networkError net.Error
		if errors.As(copyErr, &tooLarge) {
			return nil, errors.Join(resourcesaver.ErrQueuedBodyTooLarge, copyErr, closeErr)
		}
		if ctx.Err() != nil {
			return nil, errors.Join(context.Cause(ctx), copyErr, closeErr)
		}
		if errors.As(copyErr, &networkError) && networkError.Timeout() {
			return nil, errors.Join(context.DeadlineExceeded, copyErr, closeErr)
		}
		return nil, fmt.Errorf("read initiating queued body: %w", errors.Join(copyErr, closeErr))
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind initiating queued body: %w", err)
	}
	request.Body = body.File
	return body, nil
}
