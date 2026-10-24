package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"

	"github.com/mbianchidev/porto/internal/config"
	portodocker "github.com/mbianchidev/porto/internal/docker"
	"github.com/mbianchidev/porto/internal/localhttps"
	"github.com/mbianchidev/porto/internal/networking"
	"github.com/mbianchidev/porto/internal/process"
)

func (s *Server) platformRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/networking", s.getNetworking)
	mux.HandleFunc("PUT /api/networking/proxy", s.setNetworkProxy)
	mux.HandleFunc("GET /api/networking/connectivity", s.networkConnectivity)
	mux.HandleFunc("POST /api/networking/apply", s.applyEngineNetworking)
	mux.HandleFunc("POST /api/tls/https/{action}", s.localHTTPSAction)
	mux.HandleFunc("GET /api/wsl", s.wslStatus)
	mux.HandleFunc("POST /api/wsl/distributions/{name}/enable", s.enableWSL)
	mux.HandleFunc("POST /api/wsl/distributions/{name}/disable", s.disableWSL)
	mux.HandleFunc("POST /api/wsl/mounts", s.applyWSLMounts)
}

func integrationJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		http.Error(w, "invalid platform integration request: "+err.Error(), http.StatusBadRequest)
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		http.Error(w, "platform integration request must contain exactly one JSON object", http.StatusBadRequest)
		return false
	}
	return true
}

func (s *Server) getNetworking(w http.ResponseWriter, _ *http.Request) {
	certificatePath, _, err := config.CertificatePaths()
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	proxy, err := s.networking.Snapshot()
	if err != nil {
		slog.Warn("Proxy configuration is unavailable", "error", networking.Redact(err.Error()))
		proxy.Message = err.Error()
	}
	engineNetwork, engineErr := s.docker.NetworkConfigurationStatus()
	if engineErr != nil {
		slog.Warn("Engine networking state is unavailable", "error", engineErr)
	}
	routes, warnings := containerHTTPRoutes(s.docker.ContainerSnapshot())
	routeList := make([]containerHTTPRoute, 0, len(routes))
	for _, route := range routes {
		routeList = append(routeList, route)
	}
	writeJSON(w, map[string]any{
		"platform": runtime.GOOS, "https": localhttps.Snapshot(certificatePath),
		"proxy": proxy, "proxyAvailable": err == nil, "routes": routeList, "warnings": warnings,
		"engine": engineNetwork, "engineError": errorText(engineErr),
	})
}

func errorText(err error) string {
	if err != nil {
		return err.Error()
	}
	return ""
}

func (s *Server) setNetworkProxy(w http.ResponseWriter, r *http.Request) {
	var configuration networking.Config
	if !integrationJSON(w, r, &configuration) {
		return
	}
	if err := s.networking.Save(configuration); err != nil {
		writeRuntimeError(w, err)
		return
	}
	status, err := s.networking.Snapshot()
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, status)
}

func (s *Server) networkConnectivity(w http.ResponseWriter, r *http.Request) {
	host := networking.InspectHost(r.Context())
	snapshot := s.docker.ContainerSnapshot()
	capability := networking.Capability{Message: snapshot.Message}
	if snapshot.Available && !snapshot.Stale {
		backend := "local"
		if strings.Contains(strings.ToLower(snapshot.Backend), "lima") {
			backend = "lima"
		}
		if backend == "lima" {
			capability = networking.DirectIPCapability(runtime.GOOS, backend, true)
		} else {
			info, err := s.docker.BackendInfo(r.Context())
			if err != nil {
				capability.Message = "Direct-IP capability could not be verified: " + err.Error()
			} else {
				rootless := false
				for _, option := range info.SecurityOptions {
					rootless = rootless || strings.Contains(strings.ToLower(option), "rootless")
				}
				capability = networking.DirectIPCapability(runtime.GOOS, backend, rootless)
			}
		}
	}
	networks := []map[string]any{}
	if snapshot.Available && !snapshot.Stale {
		listed, err := s.docker.Networks(r.Context())
		if err != nil {
			host.Warnings = append(host.Warnings, "List workload subnets: "+err.Error())
		} else {
			for _, network := range listed {
				document, err := s.docker.InspectNetwork(r.Context(), network.ID)
				if err != nil {
					host.Warnings = append(host.Warnings, "Inspect network "+network.Name+": "+err.Error())
					continue
				}
				var inspected struct {
					IPAM struct {
						Config []struct{ Subnet string }
					}
				}
				if err := json.Unmarshal(document, &inspected); err != nil {
					host.Warnings = append(host.Warnings, "Network "+network.Name+" returned invalid subnet information")
					continue
				}
				subnets := []string{}
				for _, config := range inspected.IPAM.Config {
					if config.Subnet != "" {
						subnets = append(subnets, config.Subnet)
					}
				}
				conflicts, err := networking.SubnetConflicts(subnets, host.Subnets)
				if err != nil {
					host.Warnings = append(host.Warnings, err.Error())
				}
				networks = append(networks, map[string]any{"name": network.Name, "subnets": subnets, "overlaps": conflicts})
			}
		}
	}
	machines, err := s.vms.List(r.Context())
	if err != nil {
		host.Warnings = append(host.Warnings, "Managed VM addresses are unavailable: "+err.Error())
	}
	writeJSON(w, map[string]any{
		"host": host, "containerDirectIP": capability, "containers": snapshot.Containers,
		"networks": networks, "virtualMachines": machines,
		"vmDirectIP": networking.Capability{Message: "Reported VM guest addresses are not proof of a host route. Use the reported loopback SSH port or explicitly configured forwards; Porto does not install host routes automatically"},
	})
}

func runPlatformCommand(ctx context.Context, arguments ...string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	output, err := process.NewCommand(ctx, "", executable, arguments...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("platform integration action: %w: %s", err, output)
	}
	return nil
}

func (s *Server) localHTTPSAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	var arguments []string
	switch action {
	case "install", "uninstall":
		arguments = []string{"https", action}
	case "trust", "untrust":
		arguments = []string{"cert", action}
	default:
		http.Error(w, "HTTPS action must be install, uninstall, trust, or untrust", http.StatusBadRequest)
		return
	}
	run := s.platformCommand
	if run == nil {
		run = runPlatformCommand
	}
	if err := run(r.Context(), arguments...); err != nil {
		writeRuntimeError(w, err)
		return
	}
	s.getNetworking(w, r)
}

func (s *Server) wslStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.wsl.Snapshot(r.Context())
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, status)
}

func (s *Server) enableWSL(w http.ResponseWriter, r *http.Request) {
	var request struct {
		MountRoots []string `json:"mountRoots"`
	}
	if !integrationJSON(w, r, &request) {
		return
	}
	integration, err := s.wsl.Enable(r.Context(), r.PathValue("name"), request.MountRoots)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, map[string]any{"integration": integration, "message": "Tooling enabled only for this distribution. Open a new shell. Existing engine mounts need an explicit apply/restart"})
}

func (s *Server) disableWSL(w http.ResponseWriter, r *http.Request) {
	if err := s.wsl.Disable(r.Context(), r.PathValue("name")); err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"message": "Selected WSL tooling removed; engine data and other distributions are unchanged. Apply mounts explicitly to release old shares"})
}

func (s *Server) applyWSLMounts(w http.ResponseWriter, r *http.Request) {
	var request struct {
		RestartEngine bool `json:"restartEngine"`
	}
	if !integrationJSON(w, r, &request) {
		return
	}
	if !s.wsl.Supported() {
		http.Error(w, "selected-distribution WSL integration requires Windows", http.StatusNotImplemented)
		return
	}
	if s.hasRuntimeOperations() || !s.beginRuntimeOperation() {
		writeRuntimeError(w, fmt.Errorf("%w: finish active runtime and data operations before applying WSL mounts", portodocker.ErrConflict))
		return
	}
	defer s.endRuntimeOperation()
	if err := s.docker.ApplyWSLMounts(r.Context(), request.RestartEngine); err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"message": "Applied only selected WSL shares to the existing owned Linux engine"})
}

func (s *Server) applyEngineNetworking(w http.ResponseWriter, r *http.Request) {
	if !s.beginRuntimeOperation() {
		writeRuntimeError(w, fmt.Errorf("%w: daemon is shutting down", portodocker.ErrConflict))
		return
	}
	defer s.endRuntimeOperation()
	if err := s.docker.ApplyNetworking(r.Context()); err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"message": "Applied proxy and registry certificate settings only to the owned engine; workload trust stores were not changed"})
}
