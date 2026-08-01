package daemon

import (
	"context"
	"crypto/tls"
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
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mbianchidev/porto/internal/app"
	"github.com/mbianchidev/porto/internal/certificates"
	"github.com/mbianchidev/porto/internal/compose"
	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/discovery"
	"github.com/mbianchidev/porto/internal/gitutil"
	"github.com/mbianchidev/porto/internal/killswitch"
	"github.com/mbianchidev/porto/internal/ports"
	"github.com/mbianchidev/porto/internal/process"
	"github.com/mbianchidev/porto/internal/sendbox"
	projectsetup "github.com/mbianchidev/porto/internal/setup"
	"github.com/mbianchidev/porto/internal/sqnsl"
	"github.com/mbianchidev/porto/internal/store"
)

const (
	projectProcessExitTimeout = 5 * time.Second
	projectReadinessInterval  = 250 * time.Millisecond
	projectReadinessTimeout   = 2 * time.Second
)

var errProjectSetupConflict = errors.New("project setup conflict")

type Server struct {
	store           *store.Store
	mu              sync.Mutex
	running         map[int64]*projectProcess
	stopping        map[int64]bool
	settingUp       map[int64]bool
	deleting        map[int64]bool
	sendboxRunning  map[int64]*exec.Cmd
	sendboxStates   map[int64]string
	sendboxMessages map[int64]string
	ui              fs.FS
	sendbox         sendboxIntegration
	compose         composeIntegration
	setupRunner     projectSetupRunner
	healthClient    *http.Client
	readinessDelay  time.Duration
	sqnsl           *sqnsl.Manager
	killSwitch      *killswitch.Manager
	tlsCertificates *certificates.Manager
	userHomeDir     func() (string, error)
}

type projectProcess struct {
	cmd      *exec.Cmd
	done     chan struct{}
	project  app.Project
	stopping bool
}

type sendboxIntegration interface {
	Status(projects []app.Project) sendbox.Status
	Command(ctx context.Context, project app.Project) (*exec.Cmd, io.ReadCloser, io.ReadCloser, error)
}

type composeIntegration interface {
	Down(ctx context.Context, project app.Project) error
}

type projectSetupRunner interface {
	Run(ctx context.Context, project app.Project, emit func(stream, line string) error) (projectsetup.Result, error)
}

func New(st *store.Store, ui fs.FS) *Server {
	return &Server{
		store:           st,
		running:         map[int64]*projectProcess{},
		stopping:        map[int64]bool{},
		settingUp:       map[int64]bool{},
		deleting:        map[int64]bool{},
		sendboxRunning:  map[int64]*exec.Cmd{},
		sendboxStates:   map[int64]string{},
		sendboxMessages: map[int64]string{},
		ui:              ui,
		sendbox:         sendbox.New(nil),
		compose:         compose.New(nil),
		setupRunner:     projectsetup.ExecRunner{},
		healthClient: &http.Client{
			Timeout: projectReadinessTimeout,
			Transport: &http.Transport{
				DisableKeepAlives: true,
				Proxy:             nil,
			},
		},
		readinessDelay: projectReadinessInterval,
		sqnsl:          sqnsl.NewManager(nil),
		killSwitch:     killswitch.NewManager(nil, nil),
		userHomeDir:    os.UserHomeDir,
	}
}

func (s *Server) Run(ctx context.Context) error {
	if count, err := s.discoverCopilotWorktrees(ctx); err != nil {
		log.Printf("discover Copilot worktrees: %v", err)
	} else if count > 0 {
		log.Printf("discovered %d project(s) in Copilot worktrees", count)
	}
	certificatePath, keyPath, err := config.CertificatePaths()
	if err != nil {
		return fmt.Errorf("resolve TLS certificate paths: %w", err)
	}
	authorityPath, authorityKeyPath, err := config.CertificateAuthorityPaths()
	if err != nil {
		return fmt.Errorf("resolve TLS certificate authority paths: %w", err)
	}
	s.tlsCertificates = certificates.New(certificatePath, keyPath, authorityPath, authorityKeyPath)
	certificateStatus, err := s.ensureProjectCertificate(ctx)
	if err != nil {
		return fmt.Errorf("prepare self-signed TLS certificate: %w", err)
	}
	tlsAddress := config.RouterTLSAddress()
	mux := http.NewServeMux()
	s.routes(mux)
	srv := &http.Server{Addr: config.DaemonAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	router := &http.Server{Addr: config.RouterAddr, Handler: http.HandlerFunc(s.proxyByHost), ReadHeaderTimeout: 5 * time.Second}
	tlsRouter := &http.Server{
		Addr:              tlsAddress,
		Handler:           http.HandlerFunc(s.proxyByHost),
		ReadHeaderTimeout: 5 * time.Second,
		TLSConfig:         s.tlsCertificates.TLSConfig(),
	}
	routerListener, err := net.Listen("tcp", config.RouterAddr)
	if err != nil {
		return fmt.Errorf("listen HTTP router on %s: %w", config.RouterAddr, err)
	}
	defer routerListener.Close()
	tlsListener, err := net.Listen("tcp", tlsAddress)
	if err != nil {
		return tlsRouterListenError(tlsAddress, err)
	}
	defer tlsListener.Close()
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
		_ = router.Shutdown(context.Background())
		_ = tlsRouter.Shutdown(context.Background())
	}()
	go func() {
		if err := router.Serve(routerListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("router: %v", err)
		}
	}()
	go func() {
		if err := tlsRouter.Serve(tls.NewListener(tlsListener, tlsRouter.TLSConfig)); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
		tlsAddress,
		certificateStatus.CertificatePath,
	)
	return srv.ListenAndServe()
}

func tlsRouterListenError(address string, err error) error {
	_, rawPort, splitErr := net.SplitHostPort(address)
	port, portErr := strconv.Atoi(rawPort)
	if runtime.GOOS == "darwin" && splitErr == nil && portErr == nil && port < 1024 {
		return fmt.Errorf(
			"listen HTTPS router on %s: %w; macOS restricts ports below 1024, so use a privileged port forward or service instead of running Porto projects as root",
			address,
			err,
		)
	}
	return fmt.Errorf("listen HTTPS router on %s: %w", address, err)
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
	mux.HandleFunc("POST /api/projects/{name}/setup", s.setupProject)
	mux.HandleFunc("GET /api/projects/{name}/branches", s.branches)
	mux.HandleFunc("POST /api/projects/{name}/branch", s.branch)
	mux.HandleFunc("POST /api/projects/{name}/instances", s.createInstance)
	mux.HandleFunc("DELETE /api/projects/{name}/instance", s.removeInstance)
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

func (s *Server) renewTLS(w http.ResponseWriter, r *http.Request) {
	if s.tlsCertificates == nil {
		http.Error(w, "TLS certificate manager is not initialized", http.StatusServiceUnavailable)
		return
	}
	status, err := s.renewProjectCertificate(r.Context())
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
	if s.deleting[project.ID] {
		s.mu.Unlock()
		http.Error(w, "project instance is being deleted", http.StatusConflict)
		return
	}
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
		if _, err := s.store.UpsertProject(r.Context(), p); err != nil {
			http.Error(w, fmt.Sprintf("store discovered project %s: %v", p.Path, err), http.StatusInternalServerError)
			return
		}
	}
	if s.tlsCertificates != nil {
		if _, err := s.ensureProjectCertificate(r.Context()); err != nil {
			http.Error(w, fmt.Sprintf("refresh TLS certificate: %v", err), http.StatusInternalServerError)
			return
		}
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

func (s *Server) setupProject(w http.ResponseWriter, r *http.Request) {
	project, err := s.store.GetProject(r.Context(), r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	result, err := s.runProjectSetup(r.Context(), project)
	if errors.Is(err, errProjectSetupConflict) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, result)
}

func (s *Server) runProjectSetup(ctx context.Context, project app.Project) (projectsetup.Result, error) {
	s.mu.Lock()
	if s.deleting[project.ID] {
		s.mu.Unlock()
		return projectsetup.Result{}, fmt.Errorf("%w: %s instance is being deleted", errProjectSetupConflict, project.Name)
	}
	if s.settingUp[project.ID] {
		s.mu.Unlock()
		return projectsetup.Result{}, fmt.Errorf("%w: %s setup is already running", errProjectSetupConflict, project.Name)
	}
	if running := s.running[project.ID]; running != nil {
		s.mu.Unlock()
		return projectsetup.Result{}, fmt.Errorf("%w: stop %s before running setup", errProjectSetupConflict, project.Name)
	}
	s.settingUp[project.ID] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.settingUp, project.ID)
		s.mu.Unlock()
	}()

	setupContext := context.WithoutCancel(ctx)
	var logMu sync.Mutex
	emit := func(stream, line string) error {
		logMu.Lock()
		defer logMu.Unlock()
		return s.store.AddLog(setupContext, project.ID, stream, line)
	}
	if err := emit("system", "Dependency setup started."); err != nil {
		return projectsetup.Result{}, err
	}
	result, err := s.setupRunner.Run(setupContext, project, emit)
	if err != nil {
		_ = emit("system", "Dependency setup failed: "+err.Error())
		return result, err
	}
	if err := emit("system", "Dependency setup completed."); err != nil {
		return result, err
	}
	return result, nil
}

func (s *Server) startProject(ctx context.Context, name string, noPull bool) (app.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.store.GetProject(ctx, name)
	if err != nil {
		return p, err
	}
	if s.settingUp[p.ID] {
		return p, fmt.Errorf("%s dependency setup is running", p.Name)
	}
	if s.deleting[p.ID] {
		return p, fmt.Errorf("%s instance is being deleted", p.Name)
	}
	if s.stopping[p.ID] {
		return p, fmt.Errorf("%s is stopping", p.Name)
	}
	if running := s.running[p.ID]; running != nil && running.cmd != nil && running.cmd.Process != nil {
		p.Status = running.project.Status
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
	command := projectsetup.RuntimeCommand(p, port)
	cmd, stdout, stderr, err := process.ShellCommand(context.Background(), p.Path, command, port)
	if err != nil {
		return p, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return p, err
	}
	runtimeProject := p
	runtimeProject.Port = port
	runtimeProject.PID = cmd.Process.Pid
	runtimeProject.Status = "starting"
	running := &projectProcess{
		cmd:     cmd,
		done:    make(chan struct{}),
		project: runtimeProject,
	}
	s.running[p.ID] = running
	_ = s.store.SetRuntime(ctx, p.ID, "starting", cmd.Process.Pid, port)
	go s.captureLogs(p, "stdout", stdout)
	go s.captureLogs(p, "stderr", stderr)
	go s.waitForProject(p, running, port)
	go s.waitForProjectReady(p, running, port)
	return s.store.GetProject(ctx, name)
}

func (s *Server) waitForProjectReady(project app.Project, running *projectProcess, port int) {
	ticker := time.NewTicker(s.readinessDelay)
	defer ticker.Stop()

	for {
		select {
		case <-running.done:
			return
		case <-ticker.C:
		}

		response, err := s.healthClient.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
		if err != nil {
			continue
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			continue
		}

		s.mu.Lock()
		if s.running[project.ID] != running || running.stopping {
			s.mu.Unlock()
			return
		}
		running.project.Status = "running"
		err = s.store.SetRuntime(context.Background(), project.ID, "running", running.cmd.Process.Pid, port)
		s.mu.Unlock()
		if err != nil {
			log.Printf("mark %s ready: %v", project.Name, err)
			return
		}
		if err := s.store.AddLog(context.Background(), project.ID, "system", "HTTP readiness check passed."); err != nil {
			log.Printf("store readiness log for %s: %v", project.Name, err)
		}
		s.syncKillSwitch(context.Background())
		return
	}
}

func (s *Server) waitForProject(project app.Project, running *projectProcess, port int) {
	err := running.cmd.Wait()

	s.mu.Lock()
	current := s.running[project.ID] == running
	expectedStop := running.stopping
	status := "stopped"
	if err != nil && !expectedStop {
		status = "crashed"
	}
	var runtimeErr error
	if current {
		runtimeErr = s.store.SetRuntime(context.Background(), project.ID, status, 0, port)
		delete(s.running, project.ID)
	}
	s.mu.Unlock()

	if current && err != nil && !expectedStop {
		if logErr := s.store.AddLog(context.Background(), project.ID, "system", err.Error()); logErr != nil {
			log.Printf("store crash log for %s: %v", project.Name, logErr)
		}
	}
	if runtimeErr != nil {
		log.Printf("update runtime for %s: %v", project.Name, runtimeErr)
	}
	close(running.done)
	s.syncKillSwitch(context.Background())
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
	p, err := s.store.GetProject(ctx, name)
	if err != nil {
		return p, err
	}

	s.mu.Lock()
	if s.stopping[p.ID] {
		s.mu.Unlock()
		return p, fmt.Errorf("%s is already stopping", p.Name)
	}
	s.stopping[p.ID] = true
	running := s.running[p.ID]
	target := p
	if running != nil {
		running.stopping = true
		if running.project.ID != 0 {
			target = running.project
		}
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.stopping, p.ID)
		s.mu.Unlock()
	}()

	if force && target.Strategy == "compose" {
		operationContext := context.WithoutCancel(ctx)
		if cleanupErr := s.compose.Down(operationContext, target); cleanupErr != nil {
			return p, errors.Join(cleanupErr, forceStopProjectProcess(running))
		}
		if err := stopProjectProcess(running); err != nil {
			return p, err
		}
		if running == nil {
			if err := s.store.SetRuntime(operationContext, p.ID, "stopped", 0, p.Port); err != nil {
				return p, err
			}
		}
		return s.store.GetProject(operationContext, name)
	}

	if running != nil {
		if force {
			err = forceStopProjectProcess(running)
		} else {
			err = stopProjectProcess(running)
		}
		if err != nil {
			return p, err
		}
	}
	if err := s.store.SetRuntime(ctx, p.ID, "stopped", 0, p.Port); err != nil {
		return p, err
	}
	return s.store.GetProject(ctx, name)
}

func stopProjectProcess(running *projectProcess) error {
	if running == nil || projectProcessDone(running) {
		return nil
	}
	terminateErr := process.Terminate(running.cmd)
	if waitForProjectProcess(running, projectProcessExitTimeout) {
		return nil
	}
	killErr := process.Kill(running.cmd)
	if waitForProjectProcess(running, projectProcessExitTimeout) {
		return nil
	}
	return errors.Join(
		terminateErr,
		killErr,
		errors.New("timed out waiting for project launcher to exit"),
	)
}

func forceStopProjectProcess(running *projectProcess) error {
	if running == nil || projectProcessDone(running) {
		return nil
	}
	killErr := process.Kill(running.cmd)
	if waitForProjectProcess(running, projectProcessExitTimeout) {
		return nil
	}
	return errors.Join(killErr, errors.New("timed out waiting for project launcher to exit"))
}

func projectProcessDone(running *projectProcess) bool {
	if running == nil || running.done == nil {
		return running == nil
	}
	select {
	case <-running.done:
		return true
	default:
		return false
	}
}

func waitForProjectProcess(running *projectProcess, timeout time.Duration) bool {
	if projectProcessDone(running) {
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-running.done:
		return true
	case <-timer.C:
		return false
	}
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
	if req.Branch == gitutil.Branch(p.Path) {
		writeJSON(w, p)
		return
	}
	if err := gitutil.CanCheckout(p.Path, req.Branch); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	defaultBranch, err := gitutil.DefaultBranch(p.SourcePath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	hostname, err := s.availableBranchHostname(r.Context(), p, req.Branch, defaultBranch)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	wasRunning := p.Status == "running"
	s.mu.Lock()
	wasRunning = wasRunning || s.running[p.ID] != nil
	s.mu.Unlock()
	if wasRunning {
		if _, err := s.stopProject(r.Context(), strconv.FormatInt(p.ID, 10), false); err != nil {
			http.Error(w, fmt.Sprintf("stop before branch switch: %v", err), http.StatusInternalServerError)
			return
		}
	}
	previousBranch := gitutil.Branch(p.Path)
	if err := gitutil.Checkout(p.Path, req.Branch); err != nil {
		if wasRunning {
			if _, restartErr := s.startProject(r.Context(), strconv.FormatInt(p.ID, 10), true); restartErr != nil {
				err = errors.Join(err, fmt.Errorf("restart original branch: %w", restartErr))
			}
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.store.SetBranchIdentity(r.Context(), p.ID, req.Branch, hostname); err != nil {
		_ = gitutil.Checkout(p.Path, previousBranch)
		if wasRunning {
			_, _ = s.startProject(r.Context(), strconv.FormatInt(p.ID, 10), true)
		}
		http.Error(w, fmt.Sprintf("save branch switch: %v", err), http.StatusInternalServerError)
		return
	}
	if s.tlsCertificates != nil {
		if _, err := s.ensureProjectCertificate(r.Context()); err != nil {
			log.Printf("refresh TLS hosts after branch switch: %v", err)
		}
	}
	if wasRunning {
		if _, err := s.startProject(r.Context(), strconv.FormatInt(p.ID, 10), true); err != nil {
			http.Error(w, fmt.Sprintf("branch switched but restart failed: %v", err), http.StatusInternalServerError)
			return
		}
	}
	s.syncKillSwitch(r.Context())
	updated, err := s.store.GetProjectByID(r.Context(), p.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	updated.Branch = req.Branch
	updated.DefaultBranch = defaultBranch
	updated.HTTPSURL = config.ProjectHTTPSURL(updated.Hostname)
	writeJSON(w, updated)
}

func (s *Server) branches(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.GetProject(r.Context(), r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	branches, err := gitutil.Branches(p.SourcePath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defaultBranch, err := gitutil.DefaultBranch(p.SourcePath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"branches":      branches,
		"current":       gitutil.Branch(p.Path),
		"defaultBranch": defaultBranch,
	})
}

func (s *Server) createInstance(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Branch string `json:"branch"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Branch == "" {
		http.Error(w, "branch required", http.StatusBadRequest)
		return
	}
	project, err := s.store.GetProject(r.Context(), r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	source, err := s.store.GetProjectByPath(r.Context(), project.SourcePath)
	if err != nil {
		source = project
	}
	resolvedBranch, err := gitutil.ResolveBranch(source.Path, req.Branch)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defaultBranch, err := gitutil.DefaultBranch(source.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	worktreeRoot, err := config.ManagedWorktreeRoot()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	instance := app.Project{
		Name:            source.Name,
		SourcePath:      source.Path,
		Strategy:        source.Strategy,
		Command:         source.Command,
		BaseHostname:    source.BaseHostname,
		ManagedInstance: true,
		Branch:          req.Branch,
	}
	instance.Hostname, err = s.availableBranchHostname(r.Context(), instance, req.Branch, defaultBranch)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	worktreePath, err := gitutil.CreateWorktree(source.Path, worktreeRoot, resolvedBranch)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	instance.Path = worktreePath
	id, err := s.store.UpsertProject(r.Context(), instance)
	if err != nil {
		cleanupErr := s.rollbackInstance(r.Context(), source.Path, worktreePath, 0)
		http.Error(w, fmt.Sprintf("store branch instance: %v", errors.Join(err, cleanupErr)), http.StatusInternalServerError)
		return
	}
	created, err := s.store.GetProjectByID(r.Context(), id)
	if err != nil {
		cleanupErr := s.rollbackInstance(r.Context(), source.Path, worktreePath, id)
		http.Error(w, errors.Join(err, cleanupErr).Error(), http.StatusInternalServerError)
		return
	}
	if _, planErr := projectsetup.Plan(created); planErr == nil {
		if _, err := s.runProjectSetup(r.Context(), created); err != nil {
			cleanupErr := s.rollbackInstance(r.Context(), source.Path, worktreePath, id)
			http.Error(w, fmt.Sprintf("create branch instance: %v", errors.Join(err, cleanupErr)), http.StatusInternalServerError)
			return
		}
	} else if !errors.Is(planErr, projectsetup.ErrUnsupported) {
		cleanupErr := s.rollbackInstance(r.Context(), source.Path, worktreePath, id)
		http.Error(w, fmt.Sprintf("create branch instance: %v", errors.Join(planErr, cleanupErr)), http.StatusInternalServerError)
		return
	}
	if s.tlsCertificates != nil {
		if _, err := s.ensureProjectCertificate(r.Context()); err != nil {
			log.Printf("refresh TLS hosts after instance creation: %v", err)
		}
	}
	created.Branch = req.Branch
	created.DefaultBranch = defaultBranch
	created.HTTPSURL = config.ProjectHTTPSURL(created.Hostname)
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, created)
}

func (s *Server) removeInstance(w http.ResponseWriter, r *http.Request) {
	project, err := s.store.GetProject(r.Context(), r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if !project.ManagedInstance {
		http.Error(w, "only managed branch instances can be removed", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	alreadyDeleting := s.deleting[project.ID]
	setupRunning := s.settingUp[project.ID]
	sendboxRunning := s.sendboxRunning[project.ID] != nil
	if !alreadyDeleting && !setupRunning && !sendboxRunning {
		s.deleting[project.ID] = true
	}
	s.mu.Unlock()
	if alreadyDeleting {
		http.Error(w, "this instance is already being deleted", http.StatusConflict)
		return
	}
	if setupRunning {
		http.Error(w, "wait for dependency setup to finish before deleting this instance", http.StatusConflict)
		return
	}
	if sendboxRunning {
		http.Error(w, "stop Sendbox before deleting this instance", http.StatusConflict)
		return
	}
	defer func() {
		s.mu.Lock()
		delete(s.deleting, project.ID)
		s.mu.Unlock()
	}()
	cleanupContext := context.WithoutCancel(r.Context())
	if _, err := s.stopProject(cleanupContext, strconv.FormatInt(project.ID, 10), false); err != nil {
		http.Error(w, fmt.Sprintf("stop instance: %v", err), http.StatusInternalServerError)
		return
	}
	if err := gitutil.RemoveWorktreeForce(project.SourcePath, project.Path); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err := s.store.DeleteProject(cleanupContext, project.ID); err != nil {
		http.Error(w, fmt.Sprintf("delete instance: %v", err), http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	delete(s.sendboxStates, project.ID)
	delete(s.sendboxMessages, project.ID)
	s.mu.Unlock()
	if s.tlsCertificates != nil {
		if _, err := s.ensureProjectCertificate(r.Context()); err != nil {
			log.Printf("refresh TLS hosts after instance removal: %v", err)
		}
	}
	s.syncKillSwitch(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) rollbackInstance(ctx context.Context, sourcePath, worktreePath string, projectID int64) error {
	cleanupContext := context.WithoutCancel(ctx)
	var cleanupErr error
	if err := gitutil.RemoveWorktreeForce(sourcePath, worktreePath); err != nil {
		cleanupErr = fmt.Errorf("remove branch worktree: %w", err)
	}
	if projectID != 0 {
		if err := s.store.DeleteProject(cleanupContext, projectID); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("delete branch instance: %w", err))
		}
	}
	return cleanupErr
}

func (s *Server) availableBranchHostname(ctx context.Context, project app.Project, branch, defaultBranch string) (string, error) {
	base := project.BaseHostname
	if base == "" {
		base = project.Hostname
	}
	candidate := config.ProjectHostname(base, branch, defaultBranch)
	exists, err := s.store.HostnameExists(ctx, candidate, project.ID)
	if err != nil {
		return "", err
	}
	if !exists {
		return candidate, nil
	}
	for attempt := 0; attempt < 10; attempt++ {
		salt := branch + "|" + project.SourcePath
		if attempt > 0 {
			salt += "|" + strconv.Itoa(attempt)
		}
		candidate = config.DisambiguateProjectHostname(config.ProjectHostname(base, branch, defaultBranch), salt)
		exists, err = s.store.HostnameExists(ctx, candidate, project.ID)
		if err != nil {
			return "", err
		}
		if !exists {
			return candidate, nil
		}
	}
	return "", errors.New("cannot allocate a unique project hostname")
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
		storedBranch := ps[i].Branch
		ps[i].Branch = gitutil.Branch(ps[i].Path)
		ps[i].Dirty = gitutil.Dirty(ps[i].Path)
		if ps[i].SourcePath == "" {
			ps[i].SourcePath = ps[i].Path
		}
		ps[i].DefaultBranch, _ = gitutil.DefaultBranch(ps[i].SourcePath)
		if ps[i].DefaultBranch != "" && (storedBranch != ps[i].Branch ||
			(ps[i].Branch == ps[i].DefaultBranch && ps[i].Hostname != ps[i].BaseHostname) ||
			(ps[i].Branch != ps[i].DefaultBranch && ps[i].Hostname == ps[i].BaseHostname)) {
			hostname, hostnameErr := s.availableBranchHostname(ctx, ps[i], ps[i].Branch, ps[i].DefaultBranch)
			if hostnameErr != nil {
				return nil, fmt.Errorf("derive hostname for %s: %w", ps[i].Name, hostnameErr)
			}
			if err := s.store.SetBranchIdentity(ctx, ps[i].ID, ps[i].Branch, hostname); err != nil {
				return nil, fmt.Errorf("save branch identity for %s: %w", ps[i].Name, err)
			}
			ps[i].Hostname = hostname
		}
		ps[i].HTTPSURL = config.ProjectHTTPSURL(ps[i].Hostname)
		s.mu.Lock()
		running := s.running[ps[i].ID]
		s.mu.Unlock()
		if running != nil {
			ps[i].Status = running.project.Status
		} else if ps[i].Status == "running" || ps[i].Status == "starting" {
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
			if _, err := s.ensureProjectCertificate(ctx); err != nil {
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
		http.Error(w, "use porto.localhost or <project>.porto.localhost", http.StatusNotFound)
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
	s.mu.Lock()
	running := s.running[p.ID]
	if running == nil || running.stopping || running.cmd == nil || running.cmd.Process == nil {
		s.mu.Unlock()
		http.Error(w, "project is not running in this Porto daemon", http.StatusServiceUnavailable)
		return
	}
	port := running.project.Port
	s.mu.Unlock()
	if port <= 0 {
		http.Error(w, "project port is unavailable", http.StatusServiceUnavailable)
		return
	}
	target, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
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

func (s *Server) discoverCopilotWorktrees(ctx context.Context) (int, error) {
	home, err := s.userHomeDir()
	if err != nil {
		return 0, fmt.Errorf("resolve user home directory: %w", err)
	}
	root := filepath.Join(home, ".copilot", "copilot-worktrees")
	info, err := os.Stat(root)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("inspect %s: %w", root, err)
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("%s is not a directory", root)
	}
	found, err := discovery.Scan(ctx, []string{root}, discovery.Options{
		Depth:  config.DefaultScanDepth,
		Ignore: []string{".git", "vendor", "dist", "target"},
	})
	if err != nil {
		return 0, err
	}
	for _, project := range found {
		if _, err := s.store.UpsertProject(ctx, project); err != nil {
			return 0, fmt.Errorf("store discovered project %s: %w", project.Path, err)
		}
	}
	return len(found), nil
}

func (s *Server) ensureProjectCertificate(ctx context.Context) (certificates.Status, error) {
	hostnames, err := s.projectCertificateHostnames(ctx)
	if err != nil {
		return certificates.Status{}, err
	}
	return s.tlsCertificates.Ensure(hostnames...)
}

func (s *Server) renewProjectCertificate(ctx context.Context) (certificates.Status, error) {
	hostnames, err := s.projectCertificateHostnames(ctx)
	if err != nil {
		return certificates.Status{}, err
	}
	return s.tlsCertificates.Renew(hostnames...)
}

func (s *Server) projectCertificateHostnames(ctx context.Context) ([]string, error) {
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("list projects for TLS certificate: %w", err)
	}
	hostnames := make([]string, 0, len(projects)*2)
	for _, project := range projects {
		if strings.Contains(project.Hostname, ".") {
			hostnames = append(
				hostnames,
				project.Hostname+"."+config.LocalDomain,
				project.Hostname+"."+config.LocalhostDomain,
			)
		}
	}
	return hostnames, nil
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
