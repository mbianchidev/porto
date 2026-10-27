package docker

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

type EngineInstallProgress struct {
	Stage     string `json:"stage"`
	Component string `json:"component"`
	Message   string `json:"message"`
}

type EngineInstallFailure struct {
	Code            string `json:"code"`
	Stage           string `json:"stage,omitempty"`
	Component       string `json:"component,omitempty"`
	Message         string `json:"message"`
	CleanupRequired bool   `json:"cleanupRequired,omitempty"`
}

type engineInstallProgressKey struct{}

type engineInstallObserver struct {
	emit    func(EngineInstallProgress) error
	cancel  context.CancelCauseFunc
	current EngineInstallProgress
}

type engineInstallError struct {
	failure EngineInstallFailure
	cause   error
}

func (e *engineInstallError) Error() string { return e.failure.Message }
func (e *engineInstallError) Unwrap() error { return e.cause }

type engineInstallCleanupError struct{ error }

func (e engineInstallCleanupError) Unwrap() error { return e.error }

// InstallEngineWithProgress observes the same ownership-scoped installation as
// InstallEngine. Observer failures cancel provisioning; command output is never
// forwarded to the observer.
func (m *Manager) InstallEngineWithProgress(ctx context.Context, emit func(EngineInstallProgress) error) (Status, error) {
	if emit == nil {
		return m.InstallEngine(ctx)
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	observer := &engineInstallObserver{emit: emit, cancel: cancel}
	ctx = context.WithValue(ctx, engineInstallProgressKey{}, observer)
	status, err := m.InstallEngine(ctx)
	if err != nil {
		failure := classifyEngineInstallFailure(err, observer.current)
		return Status{}, &engineInstallError{failure: failure, cause: err}
	}
	return status, nil
}

// EngineInstallationFailure returns actionable summaries, never raw command
// output, executable paths, proxy credentials, or download URLs.
func EngineInstallationFailure(err error) EngineInstallFailure {
	var installationError *engineInstallError
	if errors.As(err, &installationError) {
		return installationError.failure
	}
	return classifyEngineInstallFailure(err, EngineInstallProgress{})
}

// EngineInstallationCause retains command and cleanup details for local
// diagnostics. Callers must redact it before logging; never send it to clients.
func EngineInstallationCause(err error) error {
	var installationError *engineInstallError
	if errors.As(err, &installationError) {
		return installationError.cause
	}
	return err
}

func engineObserver(ctx context.Context) *engineInstallObserver {
	observer, _ := ctx.Value(engineInstallProgressKey{}).(*engineInstallObserver)
	return observer
}

func reportEngineInstall(ctx context.Context, stage, component, message string) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	observer := engineObserver(ctx)
	if observer == nil {
		return nil
	}
	progress := EngineInstallProgress{Stage: stage, Component: component, Message: message}
	if observer.current == progress {
		return nil
	}
	observer.current = progress
	if err := observer.emit(progress); err != nil {
		observer.cancel(err)
		return err
	}
	return context.Cause(ctx)
}

func (m *Manager) requireEngineInstallStreaming(ctx context.Context) error {
	if engineObserver(ctx) != nil {
		if _, ok := m.runner.(streamingRunner); !ok {
			return ErrUnsupported
		}
	}
	return nil
}

func (m *Manager) streamEngineInstallCommand(ctx context.Context, timeout time.Duration, action string, command runtimes.Command) error {
	pending := map[string]string{}
	emitLine := func(line string) error {
		line = strings.ToLower(line)
		switch {
		case strings.Contains(line, "using cache"), strings.Contains(line, "using cached"):
			return reportEngineInstall(ctx, "starting", "guest-image", "Using cached Lima guest files.")
		case strings.Contains(line, "downloading"), strings.Contains(line, "fetching"), strings.HasPrefix(strings.TrimSpace(line), "get:"):
			component, message := "guest-image", "Downloading the required Lima guest image."
			if strings.Contains(line, "containerd") || strings.Contains(line, "buildkit") || strings.Contains(line, "nerdctl") || strings.Contains(line, "cni") {
				component, message = "guest-tools", "Downloading required containerd / BuildKit guest tools."
			}
			if strings.Contains(line, "qemu") || engineObserver(ctx).current.Component == "multi-platform" {
				component, message = "multi-platform", "Downloading required guest multi-platform packages."
			}
			return reportEngineInstall(ctx, "downloading", component, message)
		case strings.Contains(line, "waiting for") && (strings.Contains(line, "ssh") || strings.Contains(line, "guest") || strings.Contains(line, "probe")):
			return reportEngineInstall(ctx, "starting", "lima", "Waiting for the Lima guest and its provisioning checks.")
		}
		return context.Cause(ctx)
	}
	return m.runStreamingCommand(ctx, timeout, action, command, func(chunk runtimes.OutputChunk) error {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		text := pending[chunk.Stream] + string(chunk.Data)
		for {
			end := strings.IndexAny(text, "\r\n")
			if end < 0 {
				break
			}
			if err := emitLine(text[:end]); err != nil {
				return err
			}
			text = text[end+1:]
		}
		// Keep split log markers, not an unbounded copy of command output.
		const limit = 4096
		if len(text) > limit {
			text = text[len(text)-limit:]
		}
		pending[chunk.Stream] = text
		return emitLine(text)
	})
}

func classifyEngineInstallFailure(err error, progress EngineInstallProgress) EngineInstallFailure {
	failure := EngineInstallFailure{Code: "backend", Stage: progress.Stage, Component: progress.Component}
	message := strings.ToLower(err.Error())
	var executableError *exec.Error
	var pathError *os.PathError
	var dnsError *net.DNSError
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		failure.Code = "cancelled"
		failure.Message = "Runtime setup was interrupted or timed out. Retry when you are ready; setup has not been marked complete."
	case errors.As(err, &executableError),
		errors.As(err, &pathError) && (pathError.Op == "fork/exec" || pathError.Op == "exec"),
		progress.Component == "runtime-helper" && (errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) || strings.Contains(message, "not a regular file")),
		containsEngineInstallMessage(message, "executable file not found", "command not found", "exec format error", "bad cpu type", "not a valid win32", "lima is required", "with apt-get"),
		containsEngineInstallMessage(message, "qemu-system", "limactl", "nerdctl:", "buildctl:", "runtime helper") &&
			containsEngineInstallMessage(message, "no such file", "not found", "not executable", "cannot execute", "permission denied"):
		failure.Code = "local-tool"
		failure.Message = "A required runtime executable is missing or cannot run. Repair or reinstall Porto's bundled tools and required host prerequisites, then retry."
	case errors.As(err, &dnsError),
		containsEngineInstallMessage(message, "no such host", "name resolution", "resolve host", "could not resolve", "proxyconnect", "proxy authentication", "proxy connection", "proxy settings", "configure runtime networking", "failed to download", "download failed", "failed to fetch", "network is unreachable", "network unreachable", "certificate", "checksum mismatch", "hash mismatch", "connection reset", "temporary failure resolving", "tls handshake", "curl: (", "wget: "):
		failure.Code = "network"
		failure.Message = "Required runtime downloads or network setup failed. Check your internet connection, DNS, proxy settings and HTTPS trust, then retry."
	case containsEngineInstallMessage(message, "ownership", "not owned", "owner identifier", "does not match"):
		failure.Code = "ownership"
		failure.Message = "Porto could not verify engine ownership. Preserve existing instances and data; resolve the ownership conflict in runtime diagnostics before retrying."
	default:
		failure.Message = "The containerd / BuildKit backend could not be prepared or verified. Check runtime diagnostics and required virtualization support, then retry."
	}
	var cleanupError engineInstallCleanupError
	if errors.As(err, &cleanupError) {
		failure.CleanupRequired = true
		failure.Message += " Cleanup also needs attention; inspect runtime diagnostics before retrying."
	}
	return failure
}

func containsEngineInstallMessage(message string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(message, candidate) {
			return true
		}
	}
	return false
}
