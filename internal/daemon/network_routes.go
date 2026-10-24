package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	portodocker "github.com/mbianchidev/porto/internal/docker"
)

var routeDNSLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

type containerHTTPRoute struct {
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

func (s *Server) watchContainerRoutes(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		snapshot := s.docker.ContainerSnapshot()
		routes, warnings := containerHTTPRoutes(snapshot)
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
	current, _ := containerHTTPRoutes(s.docker.ContainerSnapshot())
	live, exists := current[hostname]
	if !exists || live.ContainerID != route.ContainerID || live.Host != route.Host || live.Port != route.Port {
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
