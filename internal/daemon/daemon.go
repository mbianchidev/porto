package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mbianchidev/porto/internal/app"
	"github.com/mbianchidev/porto/internal/certificates"
	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/discovery"
	"github.com/mbianchidev/porto/internal/gitutil"
	"github.com/mbianchidev/porto/internal/killswitch"
	"github.com/mbianchidev/porto/internal/ports"
	"github.com/mbianchidev/porto/internal/process"
	"github.com/mbianchidev/porto/internal/sendbox"
	"github.com/mbianchidev/porto/internal/sqnsl"
	"github.com/mbianchidev/porto/internal/store"
)

type Server struct {
	store           *store.Store
	mu              sync.Mutex
	running         map[int64]*exec.Cmd
	sendboxRunning  map[int64]*exec.Cmd
	sendboxStates   map[int64]string
	sendboxMessages map[int64]string
	ui              fs.FS
	sendbox         sendboxIntegration
	sqnsl           *sqnsl.Manager
	killSwitch      *killswitch.Manager
	tlsCertificates *certificates.Manager
}

type sendboxIntegration interface {
	Status(projects []app.Project) sendbox.Status
	Command(ctx context.Context, project app.Project) (*exec.Cmd, io.ReadCloser, io.ReadCloser, error)
}

func New(st *store.Store, ui fs.FS) *Server {
	return &Server{
		store:           st,
		running:         map[int64]*exec.Cmd{},
		sendboxRunning:  map[int64]*exec.Cmd{},
		sendboxStates:   map[int64]string{},
		sendboxMessages: map[int64]string{},
		ui:              ui,
		sendbox:         sendbox.New(nil),
		sqnsl:           sqnsl.NewManager(nil),
		killSwitch:      killswitch.NewManager(nil, nil),
	}
}

func (s *Server) Run(ctx context.Context) error {
	certificatePath, keyPath, err := config.CertificatePaths()
	if err != nil {
		return fmt.Errorf("resolve TLS certificate paths: %w", err)
	}
	s.tlsCertificates = certificates.New(certificatePath, keyPath)
	certificateStatus, err := s.tlsCertificates.Ensure()
	if err != nil {
		return fmt.Errorf("prepare self-signed TLS certificate: %w", err)
	}
	mux := http.NewServeMux()
	s.routes(mux)
	srv := &http.Server{Addr: config.DaemonAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	router := &http.Server{Addr: config.RouterAddr, Handler: http.HandlerFunc(s.proxyByHost), ReadHeaderTimeout: 5 * time.Second}
	tlsRouter := &http.Server{
		Addr:              config.RouterTLSAddr,
		Handler:           http.HandlerFunc(s.proxyByHost),
		ReadHeaderTimeout: 5 * time.Second,
		TLSConfig:         s.tlsCertificates.TLSConfig(),
	}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
		_ = router.Shutdown(context.Background())
		_ = tlsRouter.Shutdown(context.Background())
	}()
	go func() {
		if err := router.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("router: %v", err)
		}
	}()
	go func() {
		if err := tlsRouter.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("TLS router: %v", err)
		}
	}()
	go s.branchCleanupLoop(ctx)
	go s.certificateRenewalLoop(ctx)
	s.syncSQLNotSoLite(ctx)
	s.syncKillSwitch(ctx)
	log.Printf(
		"porto daemon listening on http://%s (routers http://%s and https://%s, certificate %s)",
		config.DaemonAddr,
		config.RouterAddr,
		config.RouterTLSAddr,
		certificateStatus.CertificatePath,
	)
	return srv.ListenAndServe()
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /api/projects", s.list)
	mux.HandleFunc("GET /api/settings", s.getSettings)
	mux.HandleFunc("PUT /api/settings", s.setSettings)
	mux.HandleFunc("GET /api/integrations/sql-not-so-lite", s.sqlNotSoLiteStatus)
	mux.HandleFunc("GET /api/integrations/kill-switch", s.killSwitchStatus)
	mux.HandleFunc("POST /api/integrations/kill-switch/install", s.installKillSwitch)
	mux.HandleFunc("POST /api/integrations/kill-switch/sync", s.syncKillSwitchNow)
	mux.HandleFunc("POST /api/integrations/kill-switch/cleanup", s.cleanupWithKillSwitch)
	mux.HandleFunc("GET /api/integrations/sendbox", s.sendboxStatus)
	mux.HandleFunc("GET /api/tls", s.tlsStatus)
	mux.HandleFunc("POST /api/tls/renew", s.renewTLS)
	mux.HandleFunc("POST /api/scan", s.scan)
	mux.HandleFunc("POST /api/projects/{name}/start", s.start)
	mux.HandleFunc("POST /api/projects/{name}/stop", s.stop(false))
	mux.HandleFunc("POST /api/projects/{name}/kill", s.stop(true))
	mux.HandleFunc("POST /api/projects/{name}/restart", s.restart)
	mux.HandleFunc("POST /api/projects/{name}/branch", s.branch)
	mux.HandleFunc("POST /api/projects/{name}/cleanup-branches", s.cleanupBranches)
	mux.HandleFunc("POST /api/projects/{name}/port", s.pinPort)
	mux.HandleFunc("POST /api/projects/{name}/sendbox/start", s.startSendbox)
	mux.HandleFunc("POST /api/projects/{name}/sendbox/stop", s.stopSendbox)
	mux.HandleFunc("GET /api/projects/{name}/logs", s.logs)
	mux.HandleFunc("POST /api/projects/{name}/logs/clear", s.clearLogs)
	mux.HandleFunc("/", s.uiHandler)
}

func (s *Server) tlsStatus(w http.ResponseWriter, _ *http.Request) {
	if s.tlsCertificates == nil {
		http.Error(w, "TLS certificate manager is not initialized", http.StatusServiceUnavailable)
		return
	}
	status, err := s.tlsCertificates.Status()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, status)
}

func (s *Server) renewTLS(w http.ResponseWriter, _ *http.Request) {
	if s.tlsCertificates == nil {
		http.Error(w, "TLS certificate manager is not initialized", http.StatusServiceUnavailable)
		return
	}
	status, err := s.tlsCertificates.Renew()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, status)
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.Settings(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, settings)
}

func (s *Server) setSettings(w http.ResponseWriter, r *http.Request) {
	var settings app.Settings
	if err := json.NewDecoder(r.Body).Decode(&settings); err != nil {
		http.Error(w, "invalid settings", http.StatusBadRequest)
		return
	}
	protected, err := gitutil.NormalizeProtectedPatterns(settings.ProtectedBranches)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	settings.ProtectedBranches = protected
	current, err := s.store.Settings(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.store.SetSettings(r.Context(), settings); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if settings.SQLNotSoLiteEnabled && !current.SQLNotSoLiteEnabled {
		s.syncSQLNotSoLite(r.Context())
	}
	if settings.KillSwitchEnabled != current.KillSwitchEnabled {
		s.syncKillSwitch(r.Context())
	}
	writeJSON(w, settings)
}

func (s *Server) sqlNotSoLiteStatus(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.Settings(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !settings.SQLNotSoLiteEnabled {
		writeJSON(w, sqnsl.Status{State: "disabled", Message: "Integration is disabled.", UpdatedAt: time.Now().UTC()})
		return
	}
	writeJSON(w, s.sqnsl.Status())
}

func (s *Server) killSwitchStatus(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.Settings(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !settings.KillSwitchEnabled {
		writeJSON(w, s.killSwitch.DisabledStatus())
		return
	}
	status := s.killSwitch.Snapshot()
	if status.State == "idle" {
		s.syncKillSwitch(r.Context())
		status = s.killSwitch.Snapshot()
	}
	writeJSON(w, status)
}

func (s *Server) installKillSwitch(w http.ResponseWriter, r *http.Request) {
	if !s.killSwitch.Snapshot().Supported {
		http.Error(w, killswitch.ErrUnsupported.Error(), http.StatusBadRequest)
		return
	}
	if !s.killSwitch.StartInstall(func(_ killswitch.Status, err error) {
		if err != nil {
			log.Printf("KillSwitch install: %v", err)
			return
		}
		s.syncKillSwitch(context.Background())
	}) {
		http.Error(w, killswitch.ErrBusy.Error(), http.StatusConflict)
		return
	}
	writeJSONStatus(w, http.StatusAccepted, s.killSwitch.Snapshot())
}

func (s *Server) syncKillSwitchNow(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.Settings(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !settings.KillSwitchEnabled {
		http.Error(w, "KillSwitch integration is disabled", http.StatusConflict)
		return
	}
	ports, err := s.activeKillSwitchPorts(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.killSwitch.RequestSync(ports, s.logKillSwitchOperation("sync")); err != nil {
		http.Error(w, err.Error(), killSwitchHTTPStatus(err))
		return
	}
	writeJSONStatus(w, http.StatusAccepted, s.killSwitch.Snapshot())
}

func (s *Server) cleanupWithKillSwitch(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.Settings(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !settings.KillSwitchEnabled {
		http.Error(w, "KillSwitch integration is disabled", http.StatusConflict)
		return
	}
	result, err := s.killSwitch.Cleanup(r.Context())
	if err != nil {
		http.Error(w, err.Error(), killSwitchHTTPStatus(err))
		return
	}
	writeJSON(w, result)
}

func (s *Server) sendboxStatus(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.Settings(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !settings.SendboxEnabled {
		writeJSON(w, sendbox.Status{State: "disabled", Message: "Integration is disabled.", UpdatedAt: time.Now().UTC()})
		return
	}
	projects, err := s.store.ListProjects(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, s.sendbox.Status(projects))
}

func (s *Server) startSendbox(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.Settings(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !settings.SendboxEnabled {
		http.Error(w, "Sendbox integration is disabled", http.StatusConflict)
		return
	}
	project, err := s.store.GetProject(r.Context(), r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	s.mu.Lock()
	if cmd := s.sendboxRunning[project.ID]; cmd != nil && cmd.Process != nil {
		s.sendboxStates[project.ID] = "running"
		s.sendboxMessages[project.ID] = "Sendbox session is running."
		s.mu.Unlock()
		s.setSendboxMetadata(&project, true)
		writeJSON(w, project)
		return
	}
	cmd, stdout, stderr, err := s.sendbox.Command(context.Background(), project)
	if err != nil {
		s.sendboxStates[project.ID] = "error"
		s.sendboxMessages[project.ID] = err.Error()
		s.mu.Unlock()
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		s.sendboxStates[project.ID] = "error"
		s.sendboxMessages[project.ID] = err.Error()
		s.mu.Unlock()
		http.Error(w, fmt.Sprintf("start Sendbox: %v", err), http.StatusInternalServerError)
		return
	}
	s.sendboxRunning[project.ID] = cmd
	s.sendboxStates[project.ID] = "running"
	s.sendboxMessages[project.ID] = "Sendbox session is running."
	s.mu.Unlock()

	_ = s.store.AddLog(r.Context(), project.ID, "system", "Sendbox session started.")
	go s.captureLogs(project, "sendbox", stdout)
	go s.captureLogs(project, "sendbox-stderr", stderr)
	go s.waitForSendbox(project, cmd)

	s.setSendboxMetadata(&project, true)
	writeJSON(w, project)
}

func (s *Server) stopSendbox(w http.ResponseWriter, r *http.Request) {
	project, err := s.store.GetProject(r.Context(), r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	s.mu.Lock()
	cmd := s.sendboxRunning[project.ID]
	if cmd == nil {
		s.mu.Unlock()
		settings, settingsErr := s.store.Settings(r.Context())
		if settingsErr != nil {
			http.Error(w, settingsErr.Error(), http.StatusInternalServerError)
			return
		}
		s.setSendboxMetadata(&project, settings.SendboxEnabled)
		writeJSON(w, project)
		return
	}
	s.sendboxStates[project.ID] = "stopping"
	s.sendboxMessages[project.ID] = "Stopping Sendbox session."
	if err := process.Terminate(cmd); err != nil {
		s.sendboxStates[project.ID] = "error"
		s.sendboxMessages[project.ID] = err.Error()
		s.mu.Unlock()
		http.Error(w, fmt.Sprintf("stop Sendbox: %v", err), http.StatusInternalServerError)
		return
	}
	s.mu.Unlock()

	s.setSendboxMetadata(&project, true)
	writeJSON(w, project)
}

func (s *Server) waitForSendbox(project app.Project, cmd *exec.Cmd) {
	err := cmd.Wait()
	s.mu.Lock()
	previous := s.sendboxStates[project.ID]
	delete(s.sendboxRunning, project.ID)
	if previous == "stopping" {
		s.sendboxStates[project.ID] = "stopped"
		s.sendboxMessages[project.ID] = "Sendbox session stopped."
	} else if err != nil {
		s.sendboxStates[project.ID] = "crashed"
		s.sendboxMessages[project.ID] = err.Error()
	} else {
		s.sendboxStates[project.ID] = "stopped"
		s.sendboxMessages[project.ID] = "Sendbox session completed."
	}
	state := s.sendboxStates[project.ID]
	message := s.sendboxMessages[project.ID]
	s.mu.Unlock()

	_ = s.store.AddLog(context.Background(), project.ID, "system", message)
	if state == "crashed" {
		log.Printf("Sendbox session for %s: %v", project.Name, err)
	}
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	projects, err := s.enriched(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, projects)
}

func (s *Server) scan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Roots  []string `json:"roots"`
		Depth  int      `json:"depth"`
		Ignore []string `json:"ignore"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if len(req.Roots) == 0 {
		http.Error(w, "roots required", http.StatusBadRequest)
		return
	}
	if req.Depth == 0 {
		req.Depth = config.DefaultScanDepth
	}
	found, err := discovery.Scan(r.Context(), req.Roots, discovery.Options{Depth: req.Depth, Ignore: req.Ignore})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, p := range found {
		_, _ = s.store.UpsertProject(r.Context(), p)
	}
	s.syncSQLNotSoLite(r.Context())
	writeJSON(w, map[string]any{"count": len(found), "projects": found})
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	noPull := r.URL.Query().Get("noPull") == "1" || r.URL.Query().Get("noPull") == "true"
	p, err := s.startProject(r.Context(), r.PathValue("name"), noPull)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.syncKillSwitch(r.Context())
	writeJSON(w, p)
}

func (s *Server) startProject(ctx context.Context, name string, noPull bool) (app.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.store.GetProject(ctx, name)
	if err != nil {
		return p, err
	}
	if cmd := s.running[p.ID]; cmd != nil && cmd.Process != nil {
		p.Status = "running"
		return p, nil
	}
	used, err := s.store.UsedPorts(ctx)
	if err != nil {
		return p, err
	}
	delete(used, p.Port)
	preferred := p.PinnedPort
	if preferred == 0 {
		preferred = p.Port
	}
	port, err := ports.Pick(preferred, config.BasePort, used)
	if err != nil {
		return p, err
	}
	if !noPull {
		if out, err := gitutil.Pull(p.Path); err != nil {
			_ = s.store.AddLog(ctx, p.ID, "git", strings.TrimSpace(out))
			return p, fmt.Errorf("git pull failed: %w", err)
		}
	}
	cmd, stdout, stderr, err := process.ShellCommand(context.Background(), p.Path, p.Command, port)
	if err != nil {
		return p, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return p, err
	}
	s.running[p.ID] = cmd
	_ = s.store.SetRuntime(ctx, p.ID, "running", cmd.Process.Pid, port)
	go s.captureLogs(p, "stdout", stdout)
	go s.captureLogs(p, "stderr", stderr)
	go func() {
		err := cmd.Wait()
		s.mu.Lock()
		delete(s.running, p.ID)
		s.mu.Unlock()
		status := "stopped"
		if err != nil {
			status = "crashed"
			_ = s.store.AddLog(context.Background(), p.ID, "system", err.Error())
		}
		_ = s.store.SetRuntime(context.Background(), p.ID, status, 0, port)
		s.syncKillSwitch(context.Background())
	}()
	return s.store.GetProject(ctx, name)
}

func (s *Server) stop(force bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := s.stopProject(r.Context(), r.PathValue("name"), force)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.syncKillSwitch(r.Context())
		writeJSON(w, p)
	}
}

func (s *Server) stopProject(ctx context.Context, name string, force bool) (app.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.store.GetProject(ctx, name)
	if err != nil {
		return p, err
	}
	cmd := s.running[p.ID]
	if cmd != nil {
		if force {
			err = process.Kill(cmd)
		} else {
			err = process.Terminate(cmd)
		}
		if err != nil {
			return p, err
		}
	}
	_ = s.store.SetRuntime(ctx, p.ID, "stopped", 0, p.Port)
	return s.store.GetProject(ctx, name)
}

func (s *Server) restart(w http.ResponseWriter, r *http.Request) {
	_, _ = s.stopProject(r.Context(), r.PathValue("name"), false)
	p, err := s.startProject(r.Context(), r.PathValue("name"), r.URL.Query().Get("noPull") == "1")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.syncKillSwitch(r.Context())
	writeJSON(w, p)
}

func (s *Server) branch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Branch string `json:"branch"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Branch == "" {
		http.Error(w, "branch required", http.StatusBadRequest)
		return
	}
	p, err := s.store.GetProject(r.Context(), r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err := gitutil.Checkout(p.Path, req.Branch); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"branch": gitutil.Branch(p.Path)})
}

func (s *Server) cleanupBranches(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.GetProject(r.Context(), r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	settings, err := s.store.Settings(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	result, err := gitutil.CleanupBranches(p.Path, settings)
	if err != nil {
		s.logCleanup(r.Context(), p, result)
		http.Error(w, cleanupError(err, result), http.StatusInternalServerError)
		return
	}
	s.logCleanup(r.Context(), p, result)
	writeJSON(w, result)
}

func (s *Server) pinPort(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Port int `json:"port"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Port <= 0 {
		http.Error(w, "port required", http.StatusBadRequest)
		return
	}
	if err := s.store.SetPinnedPort(r.Context(), r.PathValue("name"), req.Port); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]int{"port": req.Port})
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.GetProject(r.Context(), r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	stream, err := requestedLogStream(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var logs []app.LogLine
	if stream == "" {
		logs, err = s.store.Logs(r.Context(), p.ID, limit)
	} else {
		logs, err = s.store.LogsByStream(r.Context(), p.ID, stream, limit)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, logs)
}

func (s *Server) clearLogs(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.GetProject(r.Context(), r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	stream, err := requestedLogStream(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	deleted, err := s.store.ClearLogs(r.Context(), p.ID, stream)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]int64{"deleted": deleted})
}

func requestedLogStream(r *http.Request) (string, error) {
	switch stream := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("stream"))); stream {
	case "", "all":
		return "", nil
	case "stdout", "stderr":
		return stream, nil
	default:
		return "", errors.New("stream must be all, stdout, or stderr")
	}
}

func (s *Server) captureLogs(project app.Project, stream string, reader io.ReadCloser) {
	defer reader.Close()
	storeErrorLogged := false
	if err := process.Stream(reader, func(line string) error {
		if err := s.store.AddLog(context.Background(), project.ID, stream, line); err != nil {
			if !storeErrorLogged {
				log.Printf("store %s logs for %s: %v", stream, project.Name, err)
				storeErrorLogged = true
			}
		} else {
			storeErrorLogged = false
		}
		return nil
	}); err != nil {
		log.Printf("read %s logs for %s: %v", stream, project.Name, err)
	}
}

func (s *Server) enriched(ctx context.Context) ([]app.Project, error) {
	ps, err := s.store.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	settings, err := s.store.Settings(ctx)
	if err != nil {
		return nil, err
	}
	for i := range ps {
		ps[i].Branch = gitutil.Branch(ps[i].Path)
		ps[i].Dirty = gitutil.Dirty(ps[i].Path)
		s.mu.Lock()
		_, ok := s.running[ps[i].ID]
		s.mu.Unlock()
		if ok {
			ps[i].Status = "running"
		} else if ps[i].Status == "running" {
			ps[i].Status = "stopped"
		}
		s.setSendboxMetadata(&ps[i], settings.SendboxEnabled)
	}
	return ps, nil
}

func (s *Server) setSendboxMetadata(project *app.Project, enabled bool) {
	configPath, configErr := sendbox.ConfigPath(project.Path)
	project.SendboxConfigured = configPath != ""

	s.mu.Lock()
	_, running := s.sendboxRunning[project.ID]
	state := s.sendboxStates[project.ID]
	message := s.sendboxMessages[project.ID]
	s.mu.Unlock()

	if running || state == "stopping" {
		project.SendboxStatus = state
		project.SendboxMessage = message
		return
	}
	if configErr != nil {
		project.SendboxStatus = "error"
		project.SendboxMessage = configErr.Error()
		return
	}
	if !project.SendboxConfigured {
		project.SendboxStatus = "unconfigured"
		project.SendboxMessage = "Add .sendbox.yaml to enable Sendbox actions."
		return
	}
	if !enabled {
		project.SendboxStatus = "disabled"
		project.SendboxMessage = "Sendbox integration is disabled."
		return
	}
	if state != "" {
		project.SendboxStatus = state
		project.SendboxMessage = message
		return
	}
	project.SendboxStatus = "stopped"
	project.SendboxMessage = "Sendbox session has not started."
}

func (s *Server) branchCleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(config.BranchCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.cleanupAll(ctx)
		}
	}
}

func (s *Server) certificateRenewalLoop(ctx context.Context) {
	ticker := time.NewTicker(config.CertificateCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.tlsCertificates.Ensure(); err != nil {
				log.Printf("renew TLS certificate: %v", err)
			}
		}
	}
}

func (s *Server) cleanupAll(ctx context.Context) {
	settings, err := s.store.Settings(ctx)
	if err != nil {
		log.Printf("load branch cleanup settings: %v", err)
		return
	}

	if !settings.CleanupLocalMerged && !settings.CleanupRemoteMerged {
		return
	}
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		log.Printf("list projects for branch cleanup: %v", err)
		return
	}
	for _, project := range projects {
		result, err := gitutil.CleanupBranches(project.Path, settings)
		if err != nil {
			s.logCleanup(ctx, project, result)
			_ = s.store.AddLog(ctx, project.ID, "git", "branch cleanup failed: "+cleanupError(err, result))
			continue
		}
		s.logCleanup(ctx, project, result)
	}
}

func (s *Server) syncSQLNotSoLite(ctx context.Context) {
	settings, err := s.store.Settings(ctx)
	if err != nil {
		log.Printf("load sql-not-so-lite settings: %v", err)
		return
	}
	if !settings.SQLNotSoLiteEnabled {
		return
	}
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		log.Printf("list projects for sql-not-so-lite: %v", err)
		return
	}
	s.sqnsl.Start(projects, func(result sqnsl.Result, err error) {
		message := result.Output
		if err != nil {
			message = err.Error()
		} else if message == "" {
			message = "sql-not-so-lite scan completed"
		}
		for _, project := range projects {
			if containsPath(result.ProjectPaths, project.Path) {
				_ = s.store.AddLog(context.Background(), project.ID, "sqnsl", message)
			}
		}
		if err != nil {
			log.Printf("sql-not-so-lite integration: %v", err)
		}
	})
}

func (s *Server) syncKillSwitch(ctx context.Context) {
	settings, err := s.store.Settings(ctx)
	if err != nil {
		log.Printf("load KillSwitch settings: %v", err)
		return
	}
	ports := []int{}
	if settings.KillSwitchEnabled {
		ports, err = s.activeKillSwitchPorts(ctx)
		if err != nil {
			log.Printf("list active Porto ports for KillSwitch: %v", err)
			return
		}
	}
	callback := s.logKillSwitchOperation("sync")
	if !settings.KillSwitchEnabled {
		callback = func(_ killswitch.Status, err error) {
			if err != nil && !errors.Is(err, killswitch.ErrNotInstalled) && !errors.Is(err, killswitch.ErrUnsupported) {
				log.Printf("clear KillSwitch Porto ports: %v", err)
			}
		}
	}
	if err := s.killSwitch.RequestSync(ports, callback); err != nil {
		log.Printf("queue KillSwitch sync: %v", err)
	}
}

func (s *Server) activeKillSwitchPorts(ctx context.Context) ([]int, error) {
	projects, err := s.enriched(ctx)
	if err != nil {
		return nil, err
	}
	return killswitch.ManagedPorts(projects), nil
}

func (s *Server) logKillSwitchOperation(action string) func(killswitch.Status, error) {
	return func(_ killswitch.Status, err error) {
		if err != nil {
			log.Printf("KillSwitch %s: %v", action, err)
		}
	}
}

func killSwitchHTTPStatus(err error) int {
	switch {
	case errors.Is(err, killswitch.ErrUnsupported):
		return http.StatusBadRequest
	case errors.Is(err, killswitch.ErrNotInstalled), errors.Is(err, killswitch.ErrBusy):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func containsPath(paths []string, target string) bool {
	for _, path := range paths {
		if path == target {
			return true
		}
	}
	return false
}

func (s *Server) logCleanup(ctx context.Context, project app.Project, result app.BranchCleanupResult) {
	if len(result.LocalDeleted) == 0 && len(result.RemoteDeleted) == 0 {
		return
	}
	message := fmt.Sprintf("branch cleanup deleted local [%s] remote [%s]",
		strings.Join(result.LocalDeleted, ", "),
		strings.Join(result.RemoteDeleted, ", "),
	)
	_ = s.store.AddLog(ctx, project.ID, "git", message)
}

func cleanupError(err error, result app.BranchCleanupResult) string {
	if len(result.LocalDeleted) == 0 && len(result.RemoteDeleted) == 0 {
		return err.Error()
	}
	return fmt.Sprintf("%v after deleting local [%s] remote [%s]",
		err,
		strings.Join(result.LocalDeleted, ", "),
		strings.Join(result.RemoteDeleted, ", "),
	)
}

func (s *Server) proxyByHost(w http.ResponseWriter, r *http.Request) {
	hostname, local := localHostname(r.Host)
	if !local {
		http.Error(w, "use porto.local or <project>.porto.local", http.StatusNotFound)
		return
	}
	if hostname == "" {
		s.uiHandler(w, r)
		return
	}
	p, err := s.store.GetProjectByHostname(r.Context(), hostname)
	if err != nil || p.Port == 0 {
		http.Error(w, "project not found or port unknown", http.StatusNotFound)
		return
	}
	target, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", p.Port))
	httputil.NewSingleHostReverseProxy(target).ServeHTTP(w, r)
}

func localHostname(hostport string) (string, bool) {
	host := hostport
	if parsed, _, err := net.SplitHostPort(hostport); err == nil {
		host = parsed
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, domain := range []string{config.LocalDomain, config.LocalhostDomain} {
		if host == domain {
			return "", true
		}
		suffix := "." + domain
		if strings.HasSuffix(host, suffix) {
			name := strings.TrimSuffix(host, suffix)
			return name, name != ""
		}
	}
	return "", false
}

func (s *Server) uiHandler(w http.ResponseWriter, r *http.Request) {
	if s.ui != nil {
		http.FileServer(http.FS(s.ui)).ServeHTTP(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<h1>Porto</h1><p>Run <code>npm --prefix ui run build</code> to enable the React dashboard.</p>`))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func IsNotFound(err error) bool { return errors.Is(err, sql.ErrNoRows) }
