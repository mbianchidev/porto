package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	portodocker "github.com/mbianchidev/porto/internal/docker"
)

var routeDNSLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

type containerHTTPRoute struct {
	Backend     string `json:"backend"`
	ContainerID string `json:"containerId"`
	Name        string `json:"name"`
	Hostname    string `json:"hostname"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	HTTPURL     string `json:"httpURL"`
	HTTPSURL    string `json:"httpsURL"`
}

func containerHTTPRoutes(snapshot portodocker.ContainerSnapshot) (map[string]containerHTTPRoute, []string) {
	routes, warnings := map[string]containerHTTPRoute{}, []string{}
	if !snapshot.Available || snapshot.Stale {
		return routes, warnings
	}
	conflicted := map[string]bool{}
	for _, container := range snapshot.Containers {
		portValue := container.Labels["dev.porto.http-port"]
		if portValue == "" || !container.TaskPresent || !strings.EqualFold(container.State, "running") {
			continue
		}
		port, err := strconv.Atoi(portValue)
		if err != nil || port < 1 || port > 65535 {
			warnings = append(warnings, "Container "+container.Name+" has an invalid dev.porto.http-port label")
			continue
		}
		name := container.Labels["dev.porto.hostname"]
		if name == "" {
			name = strings.ReplaceAll(strings.TrimPrefix(strings.ToLower(container.Name), "/"), "_", "-")
		}
		valid := len(name) > 0 && len(name) <= 180
		for _, label := range strings.Split(name, ".") {
			valid = valid && routeDNSLabel.MatchString(label)
		}
		if !valid {
			warnings = append(warnings, "Container "+container.Name+" needs a valid lowercase dev.porto.hostname label")
			continue
		}
		hostname := name + ".docker"
		for _, mapping := range container.NetworkDetails {
			if mapping.ContainerPort != int32(port) || mapping.HostPort < 1 ||
				(mapping.Protocol != "" && mapping.Protocol != "tcp") {
				continue
			}
			host := mapping.HostIP
			switch host {
			case "", "0.0.0.0":
				host = "127.0.0.1"
			case "::":
				host = "::1"
			default:
				ip := net.ParseIP(host)
				if ip == nil || !ip.IsLoopback() {
					continue
				}
			}
			if previous, exists := routes[hostname]; exists && previous.ContainerID != container.ID {
				delete(routes, hostname)
				conflicted[hostname] = true
				warnings = append(warnings, "Multiple containers claim "+hostname+"; no ambiguous route is exposed")
			}
			if !conflicted[hostname] {
				routes[hostname] = containerHTTPRoute{
					ContainerID: container.ID, Name: container.Name, Hostname: hostname,
					Host: host, Port: int(mapping.HostPort),
					HTTPURL: config.ProjectHTTPURL(hostname), HTTPSURL: config.ProjectHTTPSURL(hostname),
				}
			}
		}
	}
	return routes, warnings
}

func backendHTTPRoutes(linux, native portodocker.ContainerSnapshot) (map[string]containerHTTPRoute, []string) {
	result, warnings := map[string]containerHTTPRoute{}, []string{}
	conflicts := map[string]bool{}
	for _, source := range []struct {
		name     string
		snapshot portodocker.ContainerSnapshot
	}{{"linux", linux}, {"windows", native}} {
		routes, messages := containerHTTPRoutes(source.snapshot)
		warnings = append(warnings, messages...)
		for hostname, route := range routes {
			if _, exists := result[hostname]; exists {
				delete(result, hostname)
				conflicts[hostname] = true
				warnings = append(warnings, "Both Linux and Windows claim "+hostname+"; choose distinct dev.porto.hostname labels")
			}
			if !conflicts[hostname] {
				route.Backend = source.name
				result[hostname] = route
			}
		}
	}
	return result, warnings
}

func (s *Server) nativeContainerSnapshot(ctx context.Context, fresh bool) portodocker.ContainerSnapshot {
	if runtime.GOOS != "windows" && s.windowsTransport == nil {
		return portodocker.ContainerSnapshot{Message: "Native Windows containers are unavailable on this host"}
	}
	s.networkRoutesMu.RLock()
	cached, updated := s.nativeRouteSnapshot, s.nativeRouteUpdated
	s.networkRoutesMu.RUnlock()
	if !fresh && time.Since(updated) < 5*time.Second {
		return cached
	}
	var snapshot portodocker.ContainerSnapshot
	if err := s.windowsJSON(ctx, "/api/docker/containers/snapshot", &snapshot); err != nil {
		snapshot = portodocker.ContainerSnapshot{Message: err.Error()}
	}
	s.networkRoutesMu.Lock()
	s.nativeRouteSnapshot, s.nativeRouteUpdated = snapshot, time.Now()
	s.networkRoutesMu.Unlock()
	return snapshot
}

func (s *Server) watchContainerRoutes(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		snapshot := s.docker.ContainerSnapshot()
		routes, warnings := backendHTTPRoutes(snapshot, s.nativeContainerSnapshot(ctx, false))
		routes, saverWarnings := s.resourceSaverRoutes(routes)
		warnings = append(warnings, saverWarnings...)
		s.networkRoutesMu.Lock()
		changed := !maps.Equal(s.networkRoutes, routes)
		s.networkRoutes = routes
		s.networkRoutesMu.Unlock()
		if changed {
			for _, warning := range warnings {
				slog.Warn("Container HTTP route unavailable", "message", warning)
			}
			if _, err := s.ensureProjectCertificate(ctx); err != nil {
				slog.Error("Refresh container HTTPS hostnames", "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) proxyContainerRoute(w http.ResponseWriter, r *http.Request, hostname string) bool {
	s.networkRoutesMu.RLock()
	route, exists := s.networkRoutes[hostname]
	s.networkRoutesMu.RUnlock()
	if !exists {
		return false
	}
	if manager := s.activeResourceSaver(); manager != nil && route.Backend != "windows" {
		state := manager.State()
		applicationID := ""
		for id, entry := range state.Entries {
			for _, owned := range entry.Application.Routes {
				if owned.Hostname == hostname && owned.ContainerID == route.ContainerID &&
					owned.Host == route.Host && owned.Port == route.Port {
					applicationID = id
				}
			}
		}
		if applicationID != "" || state.Config.Mode != "disabled" {
			if applicationID == "" {
				for _, container := range s.docker.ContainerSnapshot().Containers {
					if container.ID == route.ContainerID {
						applicationID = saverApplicationID(container)
					}
				}
			}
			if applicationID != "" {
				if r.ContentLength > state.Config.MaxBodyBytes {
					http.Error(w, "Resource Saver request exceeds the configured queued body limit", http.StatusRequestEntityTooLarge)
					return true
				}
				wakeContext, cancelWake := context.WithTimeout(r.Context(), time.Duration(state.Config.WakeSeconds)*time.Second)
				release, err := manager.Acquire(wakeContext, applicationID)
				cancelWake()
				if err != nil {
					code := http.StatusBadGateway
					if errors.Is(err, context.DeadlineExceeded) {
						code = http.StatusGatewayTimeout
					}
					slog.Warn("Resource Saver request wake failed", "application", applicationID, "error", err)
					http.Error(w, "Resource Saver could not prepare the application: "+err.Error(), code)
					return true
				}
				defer release()
				r.Body = http.MaxBytesReader(w, r.Body, state.Config.MaxBodyBytes)
			}
		}
	}
	current, _ := backendHTTPRoutes(s.docker.ContainerSnapshot(), s.nativeContainerSnapshot(r.Context(), true))
	live, exists := current[hostname]
	backend := route.Backend
	if backend == "" {
		backend = "linux"
	}
	if !exists || live.Backend != backend || live.ContainerID != route.ContainerID || live.Host != route.Host || live.Port != route.Port {
		http.Error(w, "container route ownership changed or the engine is unavailable; wait for a fresh inventory", http.StatusServiceUnavailable)
		return true
	}

	target, err := url.Parse("http://" + net.JoinHostPort(route.Host, strconv.Itoa(route.Port)))
	if err != nil {
		http.Error(w, "invalid owned container route", http.StatusInternalServerError)
		return true
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = &http.Transport{Proxy: nil, DisableKeepAlives: true, ResponseHeaderTimeout: 30 * time.Second}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		slog.Warn("Container HTTP upstream unavailable", "container", route.Name, "error", err)
		http.Error(w, fmt.Sprintf("container %s has no responding HTTP listener", route.Name), http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, r)
	return true
}

func (s *Server) resourceSaverRoutes(routes map[string]containerHTTPRoute) (map[string]containerHTTPRoute, []string) {
	manager := s.activeResourceSaver()
	if manager == nil {
		return routes, nil
	}
	conflicts := map[string]bool{}
	var warnings []string
	for _, entry := range manager.State().Entries {
		for _, owned := range entry.Application.Routes {
			if previous, exists := routes[owned.Hostname]; exists &&
				(previous.ContainerID != owned.ContainerID || previous.Backend == "windows" || previous.Host != owned.Host || previous.Port != owned.Port) {
				delete(routes, owned.Hostname)
				conflicts[owned.Hostname] = true
				warnings = append(warnings, "Resource Saver route "+owned.Hostname+" conflicts with a different live owner; no ambiguous route is exposed")
				continue
			}
			if !conflicts[owned.Hostname] {
				routes[owned.Hostname] = containerHTTPRoute{
					Backend: "linux", ContainerID: owned.ContainerID, Name: entry.Application.Name,
					Hostname: owned.Hostname, Host: owned.Host, Port: owned.Port,
					HTTPURL: config.ProjectHTTPURL(owned.Hostname), HTTPSURL: config.ProjectHTTPSURL(owned.Hostname),
				}
			}
		}
	}
	return routes, warnings
}
