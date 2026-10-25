package daemon

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	portodocker "github.com/mbianchidev/porto/internal/docker"
	"github.com/mbianchidev/porto/internal/networking"
	"github.com/mbianchidev/porto/internal/windowscontainers"
)

const windowsIntegrationPrefix = "/api/integrations/windows-containers"

func windowsContainerMode(r *http.Request) bool {
	return r.Header.Get("X-Porto-Container-Mode") == "windows" || r.URL.Query().Get("containerMode") == "windows"
}

func (s *Server) windowsContainerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+windowsIntegrationPrefix, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, windowscontainers.Snapshot(r.Context()))
	})
	mux.HandleFunc("POST "+windowsIntegrationPrefix+"/{action}", func(w http.ResponseWriter, r *http.Request) {
		action := r.PathValue("action")
		switch action {
		case "install", "start", "stop", "remove", "context":
		default:
			http.Error(w, "native Windows action must be install, start, stop, remove, or context", http.StatusBadRequest)
			return
		}
		if action == "stop" || action == "remove" || action == "context" {
			var request struct {
				Confirm bool `json:"confirm"`
			}
			if !integrationJSON(w, r, &request) {
				return
			}
			if !request.Confirm {
				http.Error(w, "this explicit native Windows runtime action requires confirm:true; native stores are preserved", http.StatusConflict)
				return
			}
		}
		if err := windowscontainers.Action(r.Context(), action); err != nil {
			code := http.StatusBadRequest
			switch {
			case errors.Is(err, windowscontainers.ErrUnsupported):
				code = http.StatusNotImplemented
			case errors.Is(err, windowscontainers.ErrOwnership):
				code = http.StatusForbidden
			case errors.Is(err, windowscontainers.ErrActive):
				code = http.StatusConflict
			}
			writeJSONStatus(w, code, map[string]any{"message": err.Error(), "status": windowscontainers.Snapshot(r.Context())})
			return
		}
		writeJSON(w, windowscontainers.Snapshot(r.Context()))
	})
}

func (s *Server) windowsClient() (*http.Client, error) {
	if s.windowsTransport != nil {
		return &http.Client{Transport: s.windowsTransport}, nil
	}
	return windowscontainers.Client()
}

func (s *Server) proxyWindowsContainerResource(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && r.URL.Path == "/api/docker/images/pull" && s.networking != nil {
		applied, err := s.syncNativeNetworking(r.Context())
		if err != nil || !applied {
			http.Error(w, "Native registry networking could not be applied; retry native start or apply networking: "+errorText(err), http.StatusServiceUnavailable)
			return
		}
	}
	if r.Method == http.MethodPost && r.URL.Path == "/api/docker/images/pull" && s.registryVault != nil && r.Header.Get("X-Registry-Auth") == "" {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRuntimeRequestBytes))
		if err != nil {
			http.Error(w, "invalid native image pull body: "+err.Error(), http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var request struct {
			Reference string `json:"reference"`
		}
		if json.Unmarshal(body, &request) == nil && request.Reference != "" {
			auth, err := s.registryAuthForImage(r.Context(), request.Reference)
			if err != nil {
				writeRuntimeError(w, err)
				return
			}
			if auth != nil {
				data, err := json.Marshal(auth)
				if err != nil {
					writeRuntimeError(w, err)
					return
				}
				r.Header.Set("X-Registry-Auth", base64.RawURLEncoding.EncodeToString(data))
			}
		}
	}
	client, err := s.windowsClient()
	if err != nil {
		http.Error(w, "The explicitly selected native Windows backend is unavailable: "+err.Error(), http.StatusNotImplemented)
		return
	}
	defer client.CloseIdleConnections()
	proxy := &httputil.ReverseProxy{
		Director: func(request *http.Request) {
			request.URL.Scheme, request.URL.Host = "http", "localhost"
			request.Host = "localhost"
			request.Header.Del("X-Porto-Container-Mode")
			query := request.URL.Query()
			query.Del("containerMode")
			request.URL.RawQuery = query.Encode()
		},
		Transport:     client.Transport,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, "Native Windows backend is not running; install/start it explicitly in Settings: "+err.Error(), http.StatusServiceUnavailable)
		},
	}
	proxy.ServeHTTP(w, r)
}

func (s *Server) windowsDockerStatus(ctx context.Context) portodocker.Status {
	status := portodocker.Status{Enabled: true, Context: windowscontainers.ContextName,
		Backend: "Porto-owned native Windows containerd/runhcs"}
	client, err := s.windowsClient()
	if err != nil {
		status.Message = err.Error()
		return status
	}
	defer client.CloseIdleConnections()
	statusContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(statusContext, http.MethodGet, "http://localhost/api/docker/status", nil)
	response, err := client.Do(request)
	if err != nil {
		status.Message = "Start the explicitly selected native Windows runtime: " + err.Error()
		return status
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		status.Message = fmt.Sprintf("native runtime status returned HTTP %d", response.StatusCode)
		return status
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1024*1024)).Decode(&status); err != nil {
		status.Message = err.Error()
	}
	return status
}

func nativeDashboardAllowed(path string) bool {
	if path == "/api/docker/status" {
		return true
	}
	for _, prefix := range []string{"/api/docker/containers", "/api/docker/images", "/api/docker/networks", "/api/docker/volumes"} {
		if path != prefix && !strings.HasPrefix(path, prefix+"/") {
			continue
		}
		parts := strings.Split(strings.Trim(strings.TrimPrefix(path, prefix), "/"), "/")
		if len(parts) >= 2 {
			switch parts[1] {
			case "files", "file", "storage", "dive", "checkpoint", "checkpoints", "restore", "update":
				return false
			}
		}
		return true
	}
	return false
}

func RunWindowsContainerWorker(ctx context.Context, sid string) error {
	return windowscontainers.RunService(ctx, sid, serveWindowsContainerWorker)
}

func serveWindowsContainerWorker(ctx context.Context, installation windowscontainers.Installation, ready chan<- struct{}) error {
	manager, err := portodocker.NewWindowsManager(nil, installation)
	if err != nil {
		return err
	}
	var networkMu sync.Mutex
	var networkGeneration string
	var networkProblem error
	var networkConfigured atomic.Bool
	applyNetwork := func(configuration networking.RuntimeConfiguration, persist bool) error {
		policy, err := networking.NewRuntime(filepath.Join(installation.Data(), "worker"), configuration)
		if err != nil {
			return err
		}
		if _, err := policy.RegistryHostsDirectory(); err != nil {
			return err
		}
		if persist {
			if err := windowscontainers.SaveNetworking(installation, configuration); err != nil {
				return err
			}
		}
		manager.SetNetworkPolicy(policy)
		networkGeneration = configuration.Generation
		networkProblem = nil
		networkConfigured.Store(true)
		return nil
	}
	if configuration, err := windowscontainers.LoadNetworking(installation); err != nil {
		networkProblem = fmt.Errorf("restore protected native networking: %w", err)
	} else if configuration != nil {
		if err := applyNetwork(*configuration, false); err != nil {
			networkProblem = err
		}
	}
	if err := manager.StartContainerInventory(ctx); err != nil {
		return err
	}
	defer func() {
		stopContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = manager.StopContainerInventory(stopContext)
	}()
	worker := &Server{docker: manager, dockerSocket: installation.Pipe(), windowsWorker: true, runtimeContext: ctx}
	mux := http.NewServeMux()
	worker.dockerResourceRoutes(mux)
	mux.HandleFunc("POST /api/worker/networking", func(w http.ResponseWriter, r *http.Request) {
		var configuration networking.RuntimeConfiguration
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*networking.MaxRegistryCABytes))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&configuration); err != nil {
			http.Error(w, "Invalid native networking configuration", http.StatusBadRequest)
			return
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			http.Error(w, "Native networking requires exactly one configuration", http.StatusBadRequest)
			return
		}
		networkMu.Lock()
		defer networkMu.Unlock()
		if err := applyNetwork(configuration, true); err != nil {
			http.Error(w, networking.Redact(err.Error()), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/worker/networking", func(w http.ResponseWriter, r *http.Request) {
		networkMu.Lock()
		defer networkMu.Unlock()
		if networkProblem != nil {
			http.Error(w, "Native registry operations are blocked until owner networking is repaired: "+networking.Redact(networkProblem.Error()), http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, map[string]any{"configured": networkConfigured.Load(), "generation": networkGeneration, "backend": "windows"})
	})
	mux.HandleFunc("GET /api/docker/containers/{id}/logs/stream", worker.dockerLogStream)
	mux.HandleFunc("GET /api/docker/containers/{id}/stats", worker.dockerInspectorStats)
	mux.HandleFunc("GET /api/worker/status", func(w http.ResponseWriter, r *http.Request) {
		host := installation.Host
		capability := func(isolation string) windowscontainers.Capability {
			_, err := host.Isolation(isolation)
			value := windowscontainers.Capability{Supported: err == nil}
			if err != nil {
				value.Reason = err.Error()
			}
			return value
		}
		status := manager.Status(r.Context(), installation.Pipe())
		networkMu.Lock()
		if networkProblem != nil {
			status.Message = "Native registry networking requires repair: " + networking.Redact(networkProblem.Error())
		}
		networkMu.Unlock()
		writeJSON(w, windowscontainers.Status{Supported: host.PotentiallySupported(), Installed: true, Running: status.Available,
			Owned: true, Context: windowscontainers.ContextName, Endpoint: installation.Endpoint(), Host: host,
			Capabilities: windowscontainers.Capabilities{Process: capability("process"), HyperV: capability("hyperv")}, Message: status.Message})
	})
	api := portodocker.NewAPI(manager, installation.Pipe())
	api.SetLifecycleContext(ctx)
	var admitted atomic.Bool
	handler := windowscontainers.Gate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/worker/status" || r.URL.Path == "/api/worker/networking" {
			mux.ServeHTTP(w, r)
			return
		}
		if !admitted.Load() {
			http.Error(w, "The native worker is validating its owned runtime; no resource mutations are admitted yet", http.StatusServiceUnavailable)
			return
		}
		if !networkConfigured.Load() &&
			(r.URL.Path == "/images/create" || r.URL.Path == "/api/docker/images/pull" || strings.HasSuffix(r.URL.Path, "/push") ||
				r.URL.Path == "/auth" || r.URL.Path == "/images/search" || strings.HasPrefix(r.URL.Path, "/distribution/")) {
			http.Error(w, "Apply networking from the owning user with 'porto networking apply' or retry native start before registry operations", http.StatusServiceUnavailable)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if !nativeDashboardAllowed(r.URL.EscapedPath()) {
				http.Error(w, "Native Windows mode does not support Linux helpers, BuildKit, checkpoints, data transfer, cleanup, or engine actions through dashboard resource routes", http.StatusNotImplemented)
				return
			}
			worker.secureHandler(mux).ServeHTTP(w, r)
			return
		}
		api.ServeHTTP(w, r)
	}))
	listener, err := windowscontainers.Listen(installation)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 30 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	// Report SCM readiness only after the owned containerd is answering.
	deadline := time.NewTimer(45 * time.Second)
	defer deadline.Stop()
	for !manager.Status(ctx, installation.Pipe()).Available {
		select {
		case <-ctx.Done():
			_ = server.Close()
			return nil
		case <-deadline.C:
			_ = server.Close()
			return errors.New("owned Windows containerd inventory did not become ready")
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err := windowscontainers.MarkAdmitted(installation); err != nil {
		_ = server.Close()
		return err
	}
	admitted.Store(true)
	close(ready)
	select {
	case <-ctx.Done():
		// Cancel streams first; Shutdown alone cannot drain hijacked exec/WS.
		_ = server.Close()
		return nil
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
