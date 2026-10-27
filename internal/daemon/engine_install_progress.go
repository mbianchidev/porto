package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/diagnostics"
	portodocker "github.com/mbianchidev/porto/internal/docker"
)

const engineInstallProgressMediaType = "application/x-ndjson"

var (
	engineInstallDiagnosticURL    = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s"'<>]+`)
	engineInstallDiagnosticBearer = regexp.MustCompile(`(?i)\bauthorization\s*:\s*bearer\s+[^\s]+`)
)

type engineInstallEvent struct {
	Type     string                             `json:"type"`
	Progress *portodocker.EngineInstallProgress `json:"progress,omitempty"`
	Status   *portodocker.Status                `json:"status,omitempty"`
	Error    *portodocker.EngineInstallFailure  `json:"error,omitempty"`
}

func requestsEngineInstallProgress(r *http.Request) bool {
	for _, accept := range strings.Split(r.Header.Get("Accept"), ",") {
		mediaType, parameters, err := mime.ParseMediaType(strings.TrimSpace(accept))
		if err != nil || !strings.EqualFold(mediaType, engineInstallProgressMediaType) {
			continue
		}
		quality := 1.0
		if value, ok := parameters["q"]; ok {
			quality, err = strconv.ParseFloat(value, 64)
		}
		if err == nil && quality > 0 && quality <= 1 {
			return true
		}
	}
	return false
}

func (s *Server) streamEngineInstallProgress(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	if s.runtimeContext != nil {
		stop := context.AfterFunc(s.runtimeContext, func() { cancel(context.Cause(s.runtimeContext)) })
		defer stop()
	}
	w.Header().Set("Content-Type", engineInstallProgressMediaType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	defer func() {
		if err := controller.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
			slog.Warn("Reset container runtime provisioning progress deadline", "error", safeEngineInstallDiagnostic(err))
		}
	}()
	encoder := json.NewEncoder(w)
	var transportError, installError error
	var progress portodocker.EngineInstallProgress
	var eventType string
	defer func() {
		if installError != nil {
			logEngineInstallFailure(installError)
		}
		cause := transportError
		if cause == nil {
			cause = context.Cause(ctx)
		}
		if cause != nil {
			slog.Warn("Container runtime provisioning progress interrupted",
				"event", eventType, "stage", progress.Stage, "runtimeComponent", progress.Component,
				"error", safeEngineInstallDiagnostic(cause))
		}
	}()
	send := func(event engineInstallEvent) error {
		eventType = event.Type
		err := context.Cause(ctx)
		if err == nil {
			err = controller.SetWriteDeadline(time.Now().Add(15 * time.Second))
			if errors.Is(err, http.ErrNotSupported) {
				err = nil
			}
		}
		if err == nil {
			err = encoder.Encode(event)
		}
		if err == nil {
			err = controller.Flush()
		}
		if err != nil {
			transportError = err
			cancel(err)
		}
		return err
	}
	status, err := s.docker.InstallEngineWithProgress(ctx, func(update portodocker.EngineInstallProgress) error {
		progress = update
		return send(engineInstallEvent{Type: "progress", Progress: &update})
	})
	installError = err
	if transportError != nil || ctx.Err() != nil {
		return
	}
	if err == nil && !status.Available {
		err = portodocker.ErrUnavailable
		installError = err
	}
	if err != nil {
		failure := portodocker.EngineInstallationFailure(err)
		_ = send(engineInstallEvent{Type: "error", Error: &failure})
		return
	}
	_ = send(engineInstallEvent{Type: "complete", Status: &status})
}

func logEngineInstallFailure(err error) {
	failure := portodocker.EngineInstallationFailure(err)
	slog.Error("Container runtime installation failed",
		"code", failure.Code, "stage", failure.Stage, "runtimeComponent", failure.Component,
		"cleanupRequired", failure.CleanupRequired,
		"error", safeEngineInstallDiagnostic(portodocker.EngineInstallationCause(err)))
}

func safeEngineInstallDiagnostic(err error) string {
	if err == nil {
		return ""
	}
	message := engineInstallDiagnosticBearer.ReplaceAllString(err.Error(), "authorization: [REDACTED]")
	output, _ := diagnostics.Redact([]byte(message), diagnostics.RedactionContext{})
	message = engineInstallDiagnosticURL.ReplaceAllString(string(output), "[REDACTED URL]")
	const limit = 16 * 1024
	if len(message) > limit {
		message = message[:limit] + " [diagnostic truncated]"
	}
	return message
}
