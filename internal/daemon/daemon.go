package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mbianchidev/porto/internal/app"
	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/discovery"
	"github.com/mbianchidev/porto/internal/gitutil"
	"github.com/mbianchidev/porto/internal/ports"
	"github.com/mbianchidev/porto/internal/process"
	"github.com/mbianchidev/porto/internal/store"
)

type Server struct {
	store   *store.Store
	mu      sync.Mutex
	running map[int64]*exec.Cmd
	ui      fs.FS
}

func New(st *store.Store, ui fs.FS) *Server {
	return &Server{store: st, running: map[int64]*exec.Cmd{}, ui: ui}
}

func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	s.routes(mux)
	srv := &http.Server{Addr: config.DaemonAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	router := &http.Server{Addr: config.RouterAddr, Handler: http.HandlerFunc(s.proxyByHost), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
		_ = router.Shutdown(context.Background())
	}()
	go func() {
		if err := router.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("router: %v", err)
		}
	}()
	log.Printf("porto daemon listening on http://%s (router http://%s)", config.DaemonAddr, config.RouterAddr)
	return srv.ListenAndServe()
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /api/projects", s.list)
	mux.HandleFunc("POST /api/scan", s.scan)
	mux.HandleFunc("POST /api/projects/{name}/start", s.start)
	mux.HandleFunc("POST /api/projects/{name}/stop", s.stop(false))
	mux.HandleFunc("POST /api/projects/{name}/kill", s.stop(true))
	mux.HandleFunc("POST /api/projects/{name}/restart", s.restart)
	mux.HandleFunc("POST /api/projects/{name}/branch", s.branch)
	mux.HandleFunc("POST /api/projects/{name}/port", s.pinPort)
	mux.HandleFunc("GET /api/projects/{name}/logs", s.logs)
	mux.HandleFunc("/", s.uiHandler)
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
	writeJSON(w, map[string]any{"count": len(found), "projects": found})
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	noPull := r.URL.Query().Get("noPull") == "1" || r.URL.Query().Get("noPull") == "true"
	p, err := s.startProject(r.Context(), r.PathValue("name"), noPull)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
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
		return p, err
	}
	s.running[p.ID] = cmd
	_ = s.store.SetRuntime(ctx, p.ID, "running", cmd.Process.Pid, port)
	go process.Stream(stdout, func(line string) { _ = s.store.AddLog(context.Background(), p.ID, "stdout", line) })
	go process.Stream(stderr, func(line string) { _ = s.store.AddLog(context.Background(), p.ID, "stderr", line) })
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
	logs, err := s.store.Logs(r.Context(), p.ID, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, logs)
}

func (s *Server) enriched(ctx context.Context) ([]app.Project, error) {
	ps, err := s.store.ListProjects(ctx)
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
	}
	return ps, nil
}

func (s *Server) proxyByHost(w http.ResponseWriter, r *http.Request) {
	host := strings.Split(r.Host, ":")[0]
	name := strings.TrimSuffix(host, ".porto.localhost")
	if name == host || name == "" {
		http.Error(w, "use <project>.porto.localhost", http.StatusNotFound)
		return
	}
	p, err := s.store.GetProject(r.Context(), name)
	if err != nil || p.Port == 0 {
		http.Error(w, "project not found or port unknown", http.StatusNotFound)
		return
	}
	target, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", p.Port))
	httputil.NewSingleHostReverseProxy(target).ServeHTTP(w, r)
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

func IsNotFound(err error) bool { return errors.Is(err, sql.ErrNoRows) }
