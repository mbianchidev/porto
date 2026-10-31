package environments

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	ProtocolVersion       = 1
	InventoryRead         = "inventory.read"
	WorkloadOperate       = "workload.operate"
	StreamsRead           = "streams.read"
	TerminalOpen          = "terminal.open"
	InfrastructureOperate = "infrastructure.operate"
	SecretsRead           = "secrets.read"
	MaxBody               = 16 << 20
)

var AllCapabilities = []string{InventoryRead, WorkloadOperate, StreamsRead, TerminalOpen, InfrastructureOperate, SecretsRead}
var safeID = regexp.MustCompile(`^[a-f0-9]{32}$`)

type Profile struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Backend       string            `json:"backend"`
	Address       string            `json:"address"`
	Identity      string            `json:"identity"`
	Certificate   string            `json:"certificate,omitempty"`
	Capabilities  []string          `json:"capabilities"`
	Labels        map[string]string `json:"labels"`
	State         string            `json:"state"`
	LastSeen      time.Time         `json:"lastSeen,omitzero"`
	LastError     string            `json:"lastError,omitempty"`
	Revision      uint64            `json:"revision"`
	Revoked       bool              `json:"revoked"`
	RemoteRevoked bool              `json:"remoteRevoked"`
	Context       string            `json:"context,omitempty"`
	Owned         bool              `json:"owned"`
	Version       string            `json:"version,omitempty"`
	Unsupported   []string          `json:"unsupported,omitempty"`
}

type AddRequest struct {
	Name              string            `json:"name"`
	Backend           string            `json:"backend"`
	Address           string            `json:"address"`
	Certificate       string            `json:"certificate"`
	EnrollmentToken   string            `json:"enrollmentToken,omitempty"`
	Token             string            `json:"token,omitempty"`
	ClientCertificate string            `json:"clientCertificate,omitempty"`
	ClientKey         string            `json:"clientKey,omitempty"`
	Kubeconfig        string            `json:"kubeconfig,omitempty"`
	Context           string            `json:"context,omitempty"`
	Operate           bool              `json:"operate,omitempty"`
	Capabilities      []string          `json:"capabilities,omitempty"`
	Labels            map[string]string `json:"labels"`
}

type secret struct {
	Token             string `json:"token,omitempty"`
	ClientCertificate string `json:"clientCertificate,omitempty"`
	ClientKey         string `json:"clientKey,omitempty"`
	Kubeconfig        string `json:"kubeconfig,omitempty"`
}

type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string                       { return e.Message }
func problem(status int, code, message string) error { return &Error{status, code, message} }

func WriteError(w http.ResponseWriter, err error) {
	value := &Error{500, "environment_failure", "Environment operation failed. Check daemon diagnostics and credential-store availability; no other target was used."}
	var known *Error
	if errors.As(err, &known) {
		value = known
	} else {
		slog.Error("Environment operation failed", "error", err)
	}
	JSON(w, value.Status, value)
}

func JSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Warn("Write environment response", "error", err)
	}
}

func Decode(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		WriteError(w, problem(400, "invalid_json", "Invalid or oversized request; use the documented fields and JSON types."))
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		WriteError(w, problem(400, "invalid_json", "Exactly one JSON request is required."))
		return false
	}
	return true
}

func ID() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

func validateCapabilities(values []string) error {
	if len(values) == 0 || len(values) > len(AllCapabilities) {
		return problem(400, "capabilities", "Select at least one supported capability.")
	}
	seen := map[string]bool{}
	for _, value := range values {
		if !slices.Contains(AllCapabilities, value) || seen[value] {
			return problem(400, "capabilities", "Capabilities must be supported and unique.")
		}
		seen[value] = true
	}
	return nil
}

func endpoint(address string) (*url.URL, error) {
	value, err := url.Parse(address)
	if err != nil || value.Scheme != "https" || value.Hostname() == "" || value.User != nil ||
		(value.Path != "" && value.Path != "/") || value.RawQuery != "" || value.Fragment != "" {
		return nil, problem(400, "endpoint", "Use an explicit HTTPS server address without credentials, path, query or fragment.")
	}
	value.Path = ""
	return value, nil
}

// This allowlist is shared by the client, the network listener and extension broker.
// A successful handshake never grants access to the rest of the local control API.
func RequiredCapability(method, target string, query url.Values) (string, error) {
	deny := func() (string, error) {
		return "", problem(403, "unsupported_route", "This API is not exposed to remote targets or extensions. Use a documented target capability.")
	}
	decoded, err := url.PathUnescape(target)
	if err != nil || decoded != path.Clean(decoded) || strings.ContainsAny(decoded, "\\\x00\r\n") || !strings.HasPrefix(target, "/api/") {
		return deny()
	}
	parts := strings.Split(strings.TrimPrefix(target, "/api/"), "/")
	for index, part := range parts {
		parts[index], err = url.PathUnescape(part)
		if err != nil {
			return deny()
		}
	}
	if len(parts) == 0 {
		return deny()
	}
	read := method == http.MethodGet || method == http.MethodHead
	switch parts[0] {
	case "health", "settings":
		if read && len(parts) == 1 {
			return InventoryRead, nil
		}
		return deny()
	case "runtime":
		if read && (len(parts) == 1 || (len(parts) == 2 && slices.Contains([]string{"features", "providers"}, parts[1]))) {
			return InventoryRead, nil
		}
		return deny()
	case "docker":
		if len(parts) < 2 {
			return deny()
		}
		if parts[1] == "engine" {
			if len(parts) == 3 && slices.Contains([]string{"install", "stop", "remove"}, parts[2]) && method == "POST" {
				return InfrastructureOperate, nil
			}
			return deny()
		}
		if !slices.Contains([]string{"status", "containers", "images", "builds", "networks", "volumes"}, parts[1]) {
			return deny()
		}
		if parts[1] == "images" {
			if len(parts) == 4 && parts[3] == "dive" {
				return TerminalOpen, nil
			}
			if method == "POST" && target != "/api/docker/images/pull" {
				return deny()
			}
			if read && len(parts) >= 3 {
				return SecretsRead, nil
			}
		}
		if parts[1] == "containers" {
			if len(parts) == 4 && slices.Contains([]string{"terminal", "exec"}, parts[3]) {
				return TerminalOpen, nil
			}
			if (len(parts) == 3 && parts[2] == "events") || (len(parts) >= 4 && parts[3] == "logs") {
				return StreamsRead, nil
			}
			if len(parts) >= 4 && slices.Contains([]string{"file", "files"}, parts[3]) {
				return deny()
			}
			if read && len(parts) == 3 && !slices.Contains([]string{"snapshot", "stats"}, parts[2]) {
				return SecretsRead, nil
			}
		}
	case "kubernetes":
		if len(parts) < 2 {
			return deny()
		}
		if parts[1] == "clusters" && !read {
			return InfrastructureOperate, nil
		}
		if parts[1] == "clusters" && len(parts) == 4 && parts[3] == "terminal" {
			return TerminalOpen, nil
		}
		if parts[1] == "pods" && len(parts) >= 5 {
			if slices.Contains([]string{"terminal", "exec", "debug"}, parts[4]) {
				return TerminalOpen, nil
			}
			if slices.Contains([]string{"file", "files"}, parts[4]) {
				return deny()
			}
			if slices.Contains([]string{"logs", "events"}, parts[4]) {
				return StreamsRead, nil
			}
		}
		if parts[1] == "secrets" || query.Get("reveal") == "true" || query.Get("resource") == "secrets" {
			return SecretsRead, nil
		}
	case "vms":
		if len(parts) < 2 || !slices.Contains([]string{"status", "images", "instances"}, parts[1]) {
			return deny()
		}
		if parts[1] == "instances" && len(parts) == 4 && slices.Contains([]string{"terminal", "exec"}, parts[3]) {
			return TerminalOpen, nil
		}
		if !read {
			return InfrastructureOperate, nil
		}
	case "projects":
		if !read && (len(parts) != 3 || !slices.Contains([]string{"start", "stop", "restart", "kill"}, parts[2])) {
			return deny()
		}
		if len(parts) == 3 && parts[2] == "logs" {
			return StreamsRead, nil
		}
	case "delivery":
		if slices.Contains(parts, "webhooks") {
			return deny()
		}
		if query.Get("reveal") == "true" || slices.Contains(parts, "definition") || slices.Contains(parts, "configuration") {
			return SecretsRead, nil
		}
	case "operations":
		if !read && (method != "POST" || !strings.HasSuffix(target, "/cancel")) {
			return deny()
		}
	case "activity":
		if !read || target != "/api/activity/resources" {
			return deny()
		}
	default:
		return deny()
	}
	if read {
		return InventoryRead, nil
	}
	if !slices.Contains([]string{"POST", "PUT", "DELETE", "PATCH"}, method) {
		return deny()
	}
	return WorkloadOperate, nil
}
