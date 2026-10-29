package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	portodocker "github.com/mbianchidev/porto/internal/docker"
	"github.com/mbianchidev/porto/internal/runtimes"
)

type engineProgressRunner struct {
	existing   bool
	created    bool
	removed    bool
	owner      string
	release    <-chan struct{}
	diagnostic string
	cleanupErr error
}

func TestEngineInstallProgressRequiresAnAcceptedStreamMediaType(t *testing.T) {
	for _, test := range []struct {
		accept string
		stream bool
	}{
		{"", false},
		{"application/json", false},
		{"application/x-ndjson", true},
		{"application/json, application/x-ndjson;q=0.5", true},
		{"application/x-ndjson;q=0", false},
		{"application/x-ndjson;q=0.000", false},
		{"application/x-ndjson;q=invalid", false},
		{"application/x-ndjson;q=2", false},
	} {
		t.Run(test.accept, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/docker/engine/install", nil)
			request.Header.Set("Accept", test.accept)
			if got := requestsEngineInstallProgress(request); got != test.stream {
				t.Fatalf("stream negotiation = %t, want %t for %q", got, test.stream, test.accept)
			}
		})
	}
}

func (r *engineProgressRunner) Run(ctx context.Context, command runtimes.Command) ([]byte, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, fmt.Errorf("command used a cancelled context: %w", err)
	}
	args := strings.Join(command.Args, " ")
	switch {
	case args == "list porto-engine --json":
		if r.existing {
			return []byte(`{"name":"porto-engine","status":"Running"}`), nil
		}
		return nil, nil
	case args == `shell --workdir=/ porto-engine -- sh -c cat "$HOME/.porto-engine-owner"`:
		return []byte("test-owner\n"), nil
	case args == `shell --workdir=/ porto-engine -- sh -c umask 077; cat > "$HOME/.porto-engine-owner"`:
		r.owner = strings.TrimSpace(string(command.Stdin))
		return nil, nil
	case args == "delete --force porto-engine":
		if r.existing {
			return nil, errors.New("attempted to delete a pre-existing engine")
		}
		if r.cleanupErr != nil {
			return nil, r.cleanupErr
		}
		r.removed = true
		return nil, nil
	case args == "version":
		return []byte("nerdctl version synthetic-fixture"), nil
	default:
		return nil, fmt.Errorf("unexpected synthetic command: %s %s", command.Name, args)
	}
}

func (r *engineProgressRunner) RunStreaming(ctx context.Context, command runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
	if command.Args[0] == "start" {
		r.created = true
	} else if !strings.Contains(strings.Join(command.Args, " "), "sudo -n sh -c") {
		return nil, errors.New("unexpected streaming command")
	}
	if err := emit(runtimes.OutputChunk{
		Stream: "stderr", Data: []byte("Downloading https://user:super-secret@mirror.invalid/?token=hidden\n"),
	}); err != nil {
		return nil, err
	}
	if r.release != nil {
		select {
		case <-r.release:
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	}
	if r.diagnostic != "" {
		return []byte(r.diagnostic), errors.New("exit status 1")
	}
	return []byte("proxyconnect tcp: 407 Proxy Authentication Required https://user:super-secret@proxy.invalid"), errors.New("exit status 1")
}

func engineProgressFixture(t *testing.T, existing bool, tools ...string) (*Server, *engineProgressRunner, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("PORTO_HOME", filepath.Join(root, "home"))
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		if runtime.GOOS == "windows" {
			tool += ".exe"
		}
		if err := os.WriteFile(filepath.Join(bin, tool), []byte("synthetic; the injected runner never executes this file"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	stateDir := filepath.Join(root, "engine")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if existing {
		if err := os.WriteFile(filepath.Join(stateDir, "engine.json"),
			[]byte(`{"mode":"lima","instance":"porto-engine","ownerId":"test-owner"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runner := &engineProgressRunner{existing: existing}
	server := &Server{docker: portodocker.NewWithStateDir(runner, stateDir), runtimeContext: context.Background()}
	return server, runner, stateDir
}

func TestEngineInstallProgressEndpointFlushesBeforeCompletion(t *testing.T) {
	server, runner, stateDir := engineProgressFixture(t, false, "limactl")
	release := make(chan struct{})
	defer close(release)
	runner.release = release
	finished := make(chan struct{})
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		server.installDockerEngine(w, r)
		close(finished)
	}))
	defer httpServer.Close()
	request, err := http.NewRequest(http.MethodPost, httpServer.URL+"/api/docker/engine/install", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept", engineInstallProgressMediaType)
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.Header.Get("Content-Type") != engineInstallProgressMediaType {
		t.Fatalf("content type = %q", response.Header.Get("Content-Type"))
	}
	decoder := json.NewDecoder(response.Body)
	for {
		var event engineInstallEvent
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event.Type != "progress" {
			t.Fatalf("completion preceded download progress: %+v", event)
		}
		if event.Progress.Stage == "downloading" {
			break
		}
	}
	select {
	case <-finished:
		t.Fatal("installation finished while the download was still pending")
	default:
	}
	release <- struct{}{}
	var failure *portodocker.EngineInstallFailure
	for {
		var event engineInstallEvent
		if err := decoder.Decode(&event); err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatal(err)
			}
			break
		}
		if event.Type == "complete" {
			t.Fatal("failed download marked complete")
		}
		if event.Type == "error" {
			failure = event.Error
		}
	}
	<-finished
	if failure == nil || failure.Code != "network" || strings.Contains(failure.Message, "super-secret") {
		t.Fatalf("unsafe or missing download failure: %+v", failure)
	}
	assertEndpointEnginePreservation(t, server, runner, stateDir, false)
}

func TestEngineInstallProgressEndpointPreservesLegacyJSONAndVerifiedDirectBackend(t *testing.T) {
	for _, streamed := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "stream"}[streamed], func(t *testing.T) {
			server, runner, _ := engineProgressFixture(t, false, "nerdctl")
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			t.Setenv("BUILDKIT_HOST", "tcp://"+listener.Addr().String())
			request := httptest.NewRequest(http.MethodPost, "/api/docker/engine/install", nil)
			if streamed {
				request.Header.Set("Accept", engineInstallProgressMediaType)
			}
			response := httptest.NewRecorder()
			server.installDockerEngine(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("install response = %d: %s", response.Code, response.Body.String())
			}
			if streamed {
				decoder := json.NewDecoder(response.Body)
				var terminal engineInstallEvent
				for {
					var event engineInstallEvent
					if err := decoder.Decode(&event); err != nil {
						if !errors.Is(err, io.EOF) {
							t.Fatal(err)
						}
						break
					}
					terminal = event
					if event.Progress != nil && event.Progress.Stage == "downloading" {
						t.Fatal("verified direct engine unnecessarily requested downloads")
					}
				}
				if terminal.Type != "complete" || terminal.Status == nil || !terminal.Status.Available {
					t.Fatalf("stream did not confirm the verified backend: %+v", terminal)
				}
			} else {
				var status portodocker.Status
				if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil || !status.Available {
					t.Fatalf("legacy status changed: %+v, %v", status, err)
				}
				if response.Header().Get("Content-Type") != "application/json" {
					t.Fatalf("legacy response type changed: %s", response.Header().Get("Content-Type"))
				}
			}
			if runner.created || runner.removed {
				t.Fatal("direct backend caused Lima provisioning")
			}
		})
	}
}

func TestEngineInstallProgressEndpointLegacyFailureRemainsJSON(t *testing.T) {
	server, _, _ := engineProgressFixture(t, false)
	response := httptest.NewRecorder()
	server.installDockerEngine(response, httptest.NewRequest(http.MethodPost, "/api/docker/engine/install", nil))
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusBadRequest || !strings.Contains(body["message"], "Lima is required") || body["type"] != "" {
		t.Fatalf("legacy error changed: %d %v", response.Code, body)
	}
}

func TestEngineInstallProgressEndpointCancellationCleansOnlyNewResources(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, shutdown := range []bool{false, true} {
			t.Run(fmt.Sprintf("existing=%t/shutdown=%t", existing, shutdown), func(t *testing.T) {
				server, runner, stateDir := engineProgressFixture(t, existing, "limactl")
				runner.release = make(chan struct{})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if shutdown {
					server.runtimeContext = ctx
				}
				request := httptest.NewRequest(http.MethodPost, "/api/docker/engine/install", nil)
				if !shutdown {
					request = request.WithContext(ctx)
				}
				request.Header.Set("Accept", engineInstallProgressMediaType)
				response := httptest.NewRecorder()
				writer := &engineProgressCancellingWriter{ResponseWriter: response, cancel: cancel}
				done := make(chan struct{})
				go func() {
					server.installDockerEngine(writer, request)
					close(done)
				}()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("cancelled transport retained provisioning work")
				}
				if strings.Contains(response.Body.String(), `"type":"complete"`) {
					t.Fatal("cancelled provisioning marked complete")
				}
				assertEndpointEnginePreservation(t, server, runner, stateDir, existing)
			})
		}
	}
}

func TestEngineInstallProgressEndpointClientDisconnectCancelsProvisioning(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "existing"}[existing], func(t *testing.T) {
			server, runner, stateDir := engineProgressFixture(t, existing, "limactl")
			runner.release = make(chan struct{})
			finished := make(chan struct{})
			httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				server.installDockerEngine(w, r)
				close(finished)
			}))
			defer httpServer.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, httpServer.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Accept", engineInstallProgressMediaType)
			client := &http.Client{Timeout: 5 * time.Second}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			decoder := json.NewDecoder(response.Body)
			for {
				var event engineInstallEvent
				if err := decoder.Decode(&event); err != nil {
					t.Fatal(err)
				}
				if event.Progress != nil && event.Progress.Stage == "downloading" {
					break
				}
			}
			cancel()
			_ = response.Body.Close()
			select {
			case <-finished:
			case <-time.After(5 * time.Second):
				t.Fatal("client disconnect did not release provisioning and cleanup")
			}
			assertEndpointEnginePreservation(t, server, runner, stateDir, existing)
		})
	}
}

type engineProgressCancellingWriter struct {
	http.ResponseWriter
	cancel context.CancelFunc
}

func (w *engineProgressCancellingWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	if bytes.Contains(data, []byte(`"stage":"downloading"`)) {
		w.cancel()
	}
	return n, err
}

func (w *engineProgressCancellingWriter) Flush() { w.ResponseWriter.(http.Flusher).Flush() }

type engineProgressFailingWriter struct {
	http.ResponseWriter
	failFlush  bool
	downloaded bool
}

func (w *engineProgressFailingWriter) Write(data []byte) (int, error) {
	w.downloaded = w.downloaded || bytes.Contains(data, []byte(`"stage":"downloading"`))
	if w.downloaded && !w.failFlush {
		return 0, errors.New("synthetic progress write failure")
	}
	return w.ResponseWriter.Write(data)
}

func (w *engineProgressFailingWriter) FlushError() error {
	if w.downloaded {
		return errors.New("synthetic progress flush failure")
	}
	w.ResponseWriter.(http.Flusher).Flush()
	return nil
}

func TestEngineInstallProgressEndpointWriteAndFlushFailuresReleaseWork(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, flush := range []bool{false, true} {
			t.Run(fmt.Sprintf("existing=%t/flush=%t", existing, flush), func(t *testing.T) {
				server, runner, stateDir := engineProgressFixture(t, existing, "limactl")
				response := httptest.NewRecorder()
				writer := &engineProgressFailingWriter{ResponseWriter: response, failFlush: flush}
				request := httptest.NewRequest(http.MethodPost, "/api/docker/engine/install", nil)
				request.Header.Set("Accept", engineInstallProgressMediaType)
				server.installDockerEngine(writer, request)
				if strings.Contains(response.Body.String(), `"type":"complete"`) {
					t.Fatal("broken progress transport marked complete")
				}
				assertEndpointEnginePreservation(t, server, runner, stateDir, existing)
			})
		}
	}
}

func TestEngineInstallProgressEndpointRefusesNewWorkDuringShutdown(t *testing.T) {
	server, runner, _ := engineProgressFixture(t, false, "limactl")
	server.runtimeClosing = true
	request := httptest.NewRequest(http.MethodPost, "/api/docker/engine/install", nil)
	request.Header.Set("Accept", engineInstallProgressMediaType)
	response := httptest.NewRecorder()
	server.installDockerEngine(response, request)
	if response.Code != http.StatusServiceUnavailable || runner.created || runner.removed {
		t.Fatalf("shutdown admitted new provisioning: %d", response.Code)
	}
}

func assertEndpointEnginePreservation(t *testing.T, server *Server, runner *engineProgressRunner, stateDir string, existing bool) {
	t.Helper()
	if server.hasRuntimeOperations() {
		t.Fatal("installation retained a runtime-operation lease")
	}
	if runner.removed == existing {
		t.Fatalf("removed=%t, existing=%t", runner.removed, existing)
	}
	contents, err := os.ReadFile(filepath.Join(stateDir, "engine.json"))
	if existing {
		if err != nil || string(contents) != `{"mode":"lima","instance":"porto-engine","ownerId":"test-owner"}` {
			t.Fatalf("pre-existing engine metadata changed: %s, %v", contents, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed new setup persisted readiness: %s, %v", contents, err)
	}
}

func captureEngineInstallDiagnostics(t *testing.T) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &output
}

func TestEngineInstallProgressLogsSanitizedSetupAndCleanupCauses(t *testing.T) {
	for _, cleanupFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("cleanupFailure=%t", cleanupFails), func(t *testing.T) {
			output := captureEngineInstallDiagnostics(t)
			server, runner, _ := engineProgressFixture(t, false, "limactl")
			runner.diagnostic = "failed to download: synthetic guest transfer refused https://downloads.invalid/guest?token=download-private-value\n" +
				"BuildKit probe failed: authorization: Bearer bearer-private-value\n"
			if cleanupFails {
				runner.cleanupErr = errors.New("synthetic cleanup blocked https://cleanup.invalid/instance?token=cleanup-private-value")
			}
			request := httptest.NewRequest(http.MethodPost, "/api/docker/engine/install", nil)
			request.Header.Set("Accept", engineInstallProgressMediaType)
			response := httptest.NewRecorder()
			server.installDockerEngine(response, request)
			logs := output.String()
			for _, expected := range []string{"Container runtime installation failed", "synthetic guest transfer refused", "exit status 1", `"code":"network"`} {
				if !strings.Contains(logs, expected) {
					t.Fatalf("missing root cause %q in diagnostics: %s", expected, logs)
				}
			}
			for _, forbidden := range []string{"downloads.invalid", "cleanup.invalid", "download-private-value", "bearer-private-value", "cleanup-private-value", "https://"} {
				if strings.Contains(logs, forbidden) {
					t.Fatalf("diagnostics exposed %q: %s", forbidden, logs)
				}
			}
			if cleanupFails && (!strings.Contains(logs, `"cleanupRequired":true`) || !strings.Contains(logs, "synthetic cleanup blocked")) {
				t.Fatalf("cleanup root cause was lost: %s", logs)
			}
			if strings.Contains(response.Body.String(), "synthetic guest transfer refused") || strings.Contains(response.Body.String(), "synthetic cleanup blocked") {
				t.Fatalf("diagnostic detail leaked into the safe UI response: %s", response.Body.String())
			}
			if server.hasRuntimeOperations() {
				t.Fatal("logging retained a runtime-operation lease")
			}
		})
	}
}

type engineProgressDiagnosticWriter struct {
	http.ResponseWriter
	failEvent string
	failFlush bool
	current   string
}

func (w *engineProgressDiagnosticWriter) Write(data []byte) (int, error) {
	var event engineInstallEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return 0, err
	}
	w.current = event.Type
	if w.current == w.failEvent && !w.failFlush {
		return 0, errors.New("synthetic progress write failure https://transport.invalid/path?token=transport-private-value")
	}
	return w.ResponseWriter.Write(data)
}

func (w *engineProgressDiagnosticWriter) FlushError() error {
	if w.current == w.failEvent && w.failFlush {
		return errors.New("synthetic progress flush failure https://transport.invalid/path?token=transport-private-value")
	}
	w.ResponseWriter.(http.Flusher).Flush()
	return nil
}

func TestEngineInstallProgressLogsTransportLossIncludingTerminalEvents(t *testing.T) {
	for _, event := range []string{"progress", "error", "complete"} {
		for _, flush := range []bool{false, true} {
			t.Run(fmt.Sprintf("event=%s/flush=%t", event, flush), func(t *testing.T) {
				output := captureEngineInstallDiagnostics(t)
				tool := "limactl"
				if event == "complete" {
					tool = "nerdctl"
				}
				server, _, _ := engineProgressFixture(t, false, tool)
				if event == "complete" {
					listener, err := net.Listen("tcp", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					defer listener.Close()
					t.Setenv("BUILDKIT_HOST", "tcp://"+listener.Addr().String())
				}
				response := httptest.NewRecorder()
				writer := &engineProgressDiagnosticWriter{ResponseWriter: response, failEvent: event, failFlush: flush}
				request := httptest.NewRequest(http.MethodPost, "/api/docker/engine/install", nil)
				request.Header.Set("Accept", engineInstallProgressMediaType)
				server.installDockerEngine(writer, request)
				logs := output.String()
				for _, expected := range []string{"Container runtime provisioning progress interrupted", "synthetic progress", `"event":"` + event + `"`} {
					if !strings.Contains(logs, expected) {
						t.Fatalf("missing transport diagnostic %q: %s", expected, logs)
					}
				}
				for _, forbidden := range []string{"transport.invalid", "transport-private-value", "https://"} {
					if strings.Contains(logs, forbidden) {
						t.Fatalf("transport diagnostic exposed %q: %s", forbidden, logs)
					}
				}
				if event == "complete" && strings.Contains(logs, "Container runtime installation failed") {
					t.Fatalf("lost acknowledgement misreported successful provisioning as failed: %s", logs)
				}
				if server.hasRuntimeOperations() {
					t.Fatal("transport-loss logging retained a runtime-operation lease")
				}
			})
		}
	}
}

func TestEngineInstallProgressLogsCancellationAndCleanupFailure(t *testing.T) {
	output := captureEngineInstallDiagnostics(t)
	server, runner, _ := engineProgressFixture(t, false, "limactl")
	runner.cleanupErr = errors.New("synthetic interrupted cleanup blocked token=cleanup-private-value")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "/api/docker/engine/install", nil).WithContext(ctx)
	request.Header.Set("Accept", engineInstallProgressMediaType)
	response := httptest.NewRecorder()
	writer := &engineProgressCancellingWriter{ResponseWriter: response, cancel: cancel}
	server.installDockerEngine(writer, request)
	logs := output.String()
	for _, expected := range []string{"Container runtime provisioning progress interrupted", "context canceled", `"cleanupRequired":true`, "synthetic interrupted cleanup blocked"} {
		if !strings.Contains(logs, expected) {
			t.Fatalf("missing cancellation/cleanup diagnostic %q: %s", expected, logs)
		}
	}
	if strings.Contains(logs, "cleanup-private-value") {
		t.Fatalf("cancelled cleanup diagnostic exposed a secret: %s", logs)
	}
}

func TestEngineInstallProgressDiagnosticRedactionPreservesUsefulFailureContext(t *testing.T) {
	diagnostic := errors.New("synthetic Buildx check failed: EACCES; password=secret-private-value\n" +
		`Details: {"token":"json-private-value"}` + "\n" +
		"Proxy check failed: authorization: Bearer bearer-private-value\n" +
		"download=https://download.invalid/tool?signature=url-private-value\n" +
		"-----BEGIN PRIVATE KEY-----\nprivate-key-value\n-----END PRIVATE KEY-----")
	message := safeEngineInstallDiagnostic(diagnostic)
	if !strings.Contains(message, "synthetic Buildx check failed: EACCES") {
		t.Fatalf("redaction hid the useful root cause: %s", message)
	}
	for _, secret := range []string{"secret-private-value", "json-private-value", "bearer-private-value", "url-private-value", "private-key-value", "download.invalid", "https://"} {
		if strings.Contains(message, secret) {
			t.Fatalf("redaction exposed %q: %s", secret, message)
		}
	}
}
