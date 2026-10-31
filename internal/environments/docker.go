package environments

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	portodocker "github.com/mbianchidev/porto/internal/docker"
)

func dockerVersion(ctx context.Context, client *http.Client, address string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, "GET", address+"/version", nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return "", problem(503, "docker_connection", "Docker TLS authentication or connectivity failed; unverified connections are not retried.")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return "", problem(502, "docker_authentication", "Docker endpoint rejected authenticated version discovery.")
	}
	var version struct {
		APIVersion    string `json:"ApiVersion"`
		MinAPIVersion string `json:"MinAPIVersion"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&version); err != nil {
		return "", problem(502, "docker_version", "Docker version discovery returned invalid JSON.")
	}
	minor := func(value string) int {
		if !strings.HasPrefix(value, "1.") {
			return -1
		}
		n, err := strconv.Atoi(strings.TrimPrefix(value, "1."))
		if err != nil {
			return -1
		}
		return n
	}
	maximum, minimum := minor(version.APIVersion), minor(version.MinAPIVersion)
	if maximum < 41 || minimum > 47 || (version.MinAPIVersion != "" && minimum < 0) {
		return "", problem(409, "docker_version", "No compatible Docker API version: this adapter supports 1.41-1.47. Connection success does not imply unsupported API support.")
	}
	return fmt.Sprintf("1.%d", min(maximum, 47)), nil
}

func dockerRequest(ctx context.Context, target *liveTarget, p Profile, method, resource string, query url.Values, body io.Reader) (*http.Response, error) {
	address, err := endpoint(p.Address)
	if err != nil {
		return nil, err
	}
	address.Path = "/v" + p.Version + resource
	address.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, method, address.String(), body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := target.transport.RoundTrip(request)
	if err != nil {
		return nil, problem(503, "docker_offline", "Selected Docker endpoint is unreachable. The action was not queued or redirected.")
	}
	if response.StatusCode >= 300 {
		response.Body.Close()
		return nil, problem(response.StatusCode, "docker_rejected", "Selected Docker endpoint rejected the request. Refresh inventory and check endpoint permissions/capability support before retrying.")
	}
	return response, nil
}

func dockerContainers(ctx context.Context, target *liveTarget, p Profile) ([]portodocker.Container, error) {
	response, err := dockerRequest(ctx, target, p, "GET", "/containers/json", url.Values{"all": {"1"}}, nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var raw []struct {
		ID      string `json:"Id"`
		Names   []string
		Image   string
		ImageID string
		State   string
		Status  string
		Created int64
		Labels  map[string]string
		Ports   []struct {
			IP          string
			PrivatePort uint16
			PublicPort  uint16
			Type        string
		}
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, MaxBody)).Decode(&raw); err != nil {
		return nil, problem(502, "docker_inventory", "Selected Docker inventory is invalid or too large.")
	}
	values := make([]portodocker.Container, 0, len(raw))
	for _, item := range raw {
		name := item.ID
		if len(item.Names) > 0 {
			name = strings.TrimPrefix(item.Names[0], "/")
		}
		ports := []string{}
		for _, p := range item.Ports {
			if p.PublicPort > 0 {
				ports = append(ports, fmt.Sprintf("%s:%d->%d/%s", p.IP, p.PublicPort, p.PrivatePort, p.Type))
			} else {
				ports = append(ports, fmt.Sprintf("%d/%s", p.PrivatePort, p.Type))
			}
		}
		values = append(values, portodocker.Container{ID: item.ID, Name: name, Image: item.Image, ImageID: item.ImageID, State: item.State, Status: item.Status, CreatedAt: time.Unix(item.Created, 0).UTC().Format(time.RFC3339), Labels: item.Labels, Ports: strings.Join(ports, ", "), TaskPresent: item.State == "running", ComposeProject: item.Labels["com.docker.compose.project"], ComposeService: item.Labels["com.docker.compose.service"], Health: portodocker.ContainerHealth{Status: "unknown"}})
	}
	return values, nil
}

func (m *Manager) dockerProxy(w http.ResponseWriter, r *http.Request, p Profile, target *liveTarget) {
	w.Header().Set("X-Porto-Environment-ID", p.ID)
	w.Header().Set("Cache-Control", "no-store")
	resource := strings.TrimPrefix(r.URL.Path, "/api/docker")
	if r.Method == "GET" && resource == "/status" {
		response, err := dockerRequest(r.Context(), target, p, "GET", "/info", nil, nil)
		if err != nil {
			m.mark(p.ID, "offline", err.Error())
			WriteError(w, err)
			return
		}
		response.Body.Close()
		m.mark(p.ID, "online", "")
		JSON(w, 200, portodocker.Status{Available: true, Enabled: true, Endpoint: p.Address, ServerVersion: "Docker API " + p.Version, Backend: "authenticated-external", Message: strings.Join(p.Unsupported, " ")})
		return
	}
	snapshot := func() (portodocker.ContainerSnapshot, error) {
		items, err := dockerContainers(r.Context(), target, p)
		value := portodocker.ContainerSnapshot{InstanceID: p.ID, Revision: uint64(time.Now().UnixNano()), Available: err == nil, Stale: err != nil, Containers: items, Backend: "authenticated-external", Message: strings.Join(p.Unsupported, " "), LastReconciledAt: time.Now().UTC()}
		reason := "This endpoint adapter does not expose Porto's native task, creation, event or checkpoint capabilities."
		no := portodocker.RuntimeCapability{Reason: reason}
		value.Capabilities = portodocker.ContainerCapabilities{DirectInventory: no, LifecycleEvents: no, DirectCreation: no, TaskRecreation: no, ExecLifecycle: no, HealthUpdates: no, NetworkUpdates: no, CheckpointRestore: no}
		return value, err
	}
	if r.Method == "GET" && slices.Contains([]string{"/containers", "/containers/snapshot", "/containers/events"}, resource) {
		if resource == "/containers/events" {
			w.Header().Set("Content-Type", "text/event-stream")
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				value, err := snapshot()
				if err != nil {
					return
				}
				data, err := json.Marshal(value)
				if err != nil {
					return
				}
				if _, err := fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", data); err != nil {
					return
				}
				if err := http.NewResponseController(w).Flush(); err != nil {
					return
				}
				select {
				case <-r.Context().Done():
					return
				case <-ticker.C:
				}
			}
		}
		value, err := snapshot()
		if err != nil {
			m.mark(p.ID, "offline", err.Error())
			WriteError(w, err)
			return
		}
		m.mark(p.ID, "online", "")
		if resource == "/containers" {
			JSON(w, 200, value.Containers)
		} else {
			JSON(w, 200, value)
		}
		return
	}
	parts := strings.Split(strings.Trim(resource, "/"), "/")
	method, remote, query := r.Method, "", url.Values{}
	if len(parts) == 2 && parts[0] == "containers" && r.Method == "GET" {
		remote = "/containers/" + parts[1] + "/json"
	}
	if len(parts) == 3 && parts[0] == "containers" {
		switch parts[2] {
		case "start", "stop", "restart", "pause", "unpause", "kill":
			if r.Method == "POST" {
				remote = resource
			}
		case "remove", "remove-force":
			if r.Method == "POST" {
				method, remote = "DELETE", "/containers/"+parts[1]
				query.Set("force", strconv.FormatBool(parts[2] == "remove-force"))
				query.Set("v", "false")
			}
		case "logs":
			if r.Method == "GET" {
				remote = resource
				query.Set("stdout", "true")
				query.Set("stderr", "true")
				query.Set("tail", "500")
			}
		case "identity":
			if r.Method == "GET" {
				remote = "/containers/" + parts[1] + "/json"
			}
		}
	}
	if remote == "" {
		WriteError(w, problem(501, "docker_unsupported", "This compatible-endpoint adapter supports container inventory, inspection, bounded logs and explicit lifecycle actions only. Porto builds, exec/terminals, host operations and storage APIs are not silently emulated."))
		return
	}
	response, err := dockerRequest(r.Context(), target, p, method, remote, query, nil)
	if err != nil {
		WriteError(w, err)
		return
	}
	defer response.Body.Close()
	if len(parts) == 3 && parts[2] == "identity" {
		var identity struct {
			ID    string `json:"Id"`
			State struct {
				Status  string
				Running bool
				Paused  bool
			}
		}
		if err := json.NewDecoder(io.LimitReader(response.Body, MaxBody)).Decode(&identity); err != nil {
			WriteError(w, problem(502, "docker_identity", "Selected container identity is invalid."))
			return
		}
		JSON(w, 200, identity)
		return
	}
	if strings.HasSuffix(remote, "/logs") {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if err := copyDockerLogs(w, response.Body); err != nil {
			m.mark(p.ID, "stale", "Container log transfer was interrupted; refresh before retrying.")
		}
		return
	}
	if r.Method != "GET" {
		JSON(w, 200, map[string]string{"status": "completed", "environmentId": p.ID})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := io.Copy(w, io.LimitReader(response.Body, MaxBody)); err != nil {
		m.mark(p.ID, "stale", "Docker response transfer was interrupted.")
	}
}

func copyDockerLogs(w io.Writer, source io.Reader) error {
	reader := bufio.NewReader(io.LimitReader(source, 1<<20))
	for {
		header, err := reader.Peek(8)
		if err == io.EOF {
			_, err = io.Copy(w, reader)
			return err
		}
		if err != nil {
			return err
		}
		if header[0] > 2 || header[1] != 0 || header[2] != 0 || header[3] != 0 {
			_, err := io.Copy(w, reader)
			return err
		}
		size := binary.BigEndian.Uint32(header[4:])
		if size > 1<<20 {
			return fmt.Errorf("Docker log frame exceeds bounded output")
		}
		if _, err := reader.Discard(8); err != nil {
			return err
		}
		if _, err := io.CopyN(w, reader, int64(size)); err != nil {
			return err
		}
	}
}
