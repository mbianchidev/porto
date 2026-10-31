package environments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/credentials"
)

const vaultService = "dev.mbianchi.porto.environments"

type state struct {
	Version  int                   `json:"version"`
	Profiles map[string]Profile    `json:"profiles"`
	Peers    map[string]peer       `json:"peers"`
	Invites  map[string]invitation `json:"invites"`
	Listener *ListenerOptions      `json:"listener,omitempty"`
}

type liveTarget struct {
	ctx       context.Context
	cancel    context.CancelFunc
	transport http.RoundTripper
	close     func() error
}

type Manager struct {
	root         string
	vault        credentials.Store
	account      string
	mu           sync.Mutex
	state        state
	live         map[string]*liveTarget
	peerContexts map[string]*liveTarget
	now          func() time.Time
	listener     *http.Server
	listenerStop func() bool
	closed       bool
}

func Open(root string, vault credentials.Store) (*Manager, error) {
	if vault == nil {
		vault = credentials.System{}
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256([]byte(absolute))
	m := &Manager{root: absolute, vault: credentials.NewChunked(vault), account: hex.EncodeToString(hash[:8]), now: time.Now, live: map[string]*liveTarget{}, peerContexts: map[string]*liveTarget{}, state: state{Version: 1, Profiles: map[string]Profile{}, Peers: map[string]peer{}, Invites: map[string]invitation{}}}
	data, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err == nil {
		if err := json.Unmarshal(data, &m.state); err != nil {
			return nil, fmt.Errorf("read environment state: %w", err)
		}
		if m.state.Version != 1 || m.state.Profiles == nil || m.state.Peers == nil || m.state.Invites == nil {
			return nil, errors.New("unsupported or incomplete environment state; restore a valid state file")
		}
		for id, p := range m.state.Profiles {
			if !safeID.MatchString(id) || p.ID != id {
				return nil, errors.New("invalid persisted environment identity")
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return m, nil
}

func (m *Manager) saveLocked() error {
	data, err := json.Marshal(m.state)
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(filepath.Join(m.root, "state.json"), data, 0o600)
}

func (m *Manager) setSecret(id string, value secret) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := m.vault.Set(vaultService, m.account+":"+id, string(data)); err != nil {
		return fmt.Errorf("save environment credential in OS credential store: %w", err)
	}
	return nil
}
func (m *Manager) getSecret(id string) (secret, error) {
	var value secret
	data, err := m.vault.Get(vaultService, m.account+":"+id)
	if err != nil {
		return value, fmt.Errorf("read environment credential from OS credential store: %w", err)
	}
	err = json.Unmarshal([]byte(data), &value)
	return value, err
}

func localProfile() Profile {
	return Profile{ID: "local", Name: "This machine", Backend: "local", Address: "loopback", Identity: "local OS user", Capabilities: slices.Clone(AllCapabilities), Labels: map[string]string{}, State: "online", Revision: 1, Owned: true}
}

func (m *Manager) List() []Profile {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := []Profile{localProfile()}
	for _, p := range m.state.Profiles {
		p.Capabilities = slices.Clone(p.Capabilities)
		p.Labels = cloneLabels(p.Labels)
		if p.Revoked {
			p.State = "revoked"
		} else if p.Backend == "edge" && m.live[p.ID] == nil {
			p.State = "offline"
		} else if p.State == "online" && m.now().Sub(p.LastSeen) > 30*time.Second {
			p.State = "stale"
		}
		result = append(result, p)
	}
	slices.SortFunc(result[1:], func(a, b Profile) int { return strings.Compare(a.Name+a.ID, b.Name+b.ID) })
	return result
}
func cloneLabels(input map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range input {
		out[k] = v
	}
	return out
}
func (m *Manager) Profile(id string) (Profile, error) {
	if id == "local" {
		return localProfile(), nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	value, ok := m.state.Profiles[id]
	if !ok {
		return value, problem(404, "target_not_found", "Environment not found. Select an enrolled target; the local machine is never a fallback.")
	}
	value.Capabilities = slices.Clone(value.Capabilities)
	value.Labels = cloneLabels(value.Labels)
	return value, nil
}

func (m *Manager) Add(ctx context.Context, request AddRequest) (Profile, error) {
	if err := validateMetadata(request.Name, request.Labels); err != nil {
		return Profile{}, err
	}
	id, err := ID()
	if err != nil {
		return Profile{}, err
	}
	profile := Profile{ID: id, Name: strings.TrimSpace(request.Name), Backend: request.Backend, Address: request.Address, Certificate: request.Certificate, Labels: cloneLabels(request.Labels), Revision: 1, State: "offline"}
	if request.Backend == "porto" || request.Backend == "docker" {
		address, err := endpoint(request.Address)
		if err != nil {
			return Profile{}, err
		}
		profile.Address = address.String()
	}
	value := secret{Token: request.Token, ClientCertificate: request.ClientCertificate, ClientKey: request.ClientKey}
	switch request.Backend {
	case "porto":
		if len(request.Capabilities) > 0 {
			return Profile{}, problem(400, "enrollment_capabilities", "Porto capabilities are assigned by the remote host's enrollment, never overridden by a local profile.")
		}
		client, err := secureClient(profile, value)
		if err != nil {
			return Profile{}, err
		}
		defer client.CloseIdleConnections()
		var enrollment enrollmentResponse
		if err := exchange(ctx, client, profile.Address+"/v1/enroll", map[string]string{"token": request.EnrollmentToken, "kind": "client"}, &enrollment); err != nil {
			return Profile{}, err
		}
		if enrollment.Version != ProtocolVersion || enrollment.Kind != "client" || enrollment.Identity == "" || len(enrollment.Token) != 64 {
			return Profile{}, problem(502, "protocol", "The remote agent returned an incompatible enrollment response.")
		}
		profile.Identity, profile.Capabilities = enrollment.Identity, enrollment.Capabilities
		value.Token = enrollment.Token
	case "docker":
		if value.Token == "" && (value.ClientCertificate == "" || value.ClientKey == "") {
			return Profile{}, problem(400, "authentication", "A compatible Docker endpoint requires an explicit bearer credential or client certificate/key.")
		}
		client, err := secureClient(profile, value)
		if err != nil {
			return Profile{}, err
		}
		defer client.CloseIdleConnections()
		version, err := dockerVersion(ctx, client, profile.Address)
		if err != nil {
			return Profile{}, err
		}
		profile.Version, profile.Identity = version, "TLS server certificate and explicit client credential"
		profile.Capabilities = []string{InventoryRead, StreamsRead}
		if request.Operate {
			profile.Capabilities = append(profile.Capabilities, WorkloadOperate)
		}
		profile.Unsupported = []string{"Porto host provisioning, builds, storage automation, native mounts, migration, image analysis and terminals are not exposed by the compatible-endpoint adapter. Docker API 1.41-1.47 inventory, logs and explicit lifecycle actions only; Swarm and plugins are not supported."}
	case "kubernetes":
		data, address, identity, contextName, err := ImportKubeconfig(request.Kubeconfig, request.Context)
		if err != nil {
			return Profile{}, err
		}
		value.Kubeconfig = string(data)
		profile.Address, profile.Identity, profile.Context, profile.Certificate = address, identity, contextName, ""
		profile.Capabilities = []string{InventoryRead, StreamsRead}
		if request.Operate {
			profile.Capabilities = append(profile.Capabilities, WorkloadOperate)
		}
		profile.Unsupported = []string{"Imported infrastructure is not owned: cluster provisioning, deletion, upgrades, node provisioning and host filesystem access are never authorized by import."}
	default:
		return Profile{}, problem(400, "backend", "Backend must be porto, docker or kubernetes. Edge targets are created only by explicit enrollment.")
	}
	if request.Backend == "kubernetes" || request.Backend == "docker" {
		for _, capability := range request.Capabilities {
			if capability != SecretsRead && !(request.Backend == "kubernetes" && capability == TerminalOpen) {
				return Profile{}, problem(400, "import_capabilities", "Additional imported capabilities are secrets.read and, for Kubernetes only, terminal.open. Workload operations use the separate explicit operate consent.")
			}
			if !slices.Contains(profile.Capabilities, capability) {
				profile.Capabilities = append(profile.Capabilities, capability)
			}
		}
	}
	if err := validateCapabilities(profile.Capabilities); err != nil {
		return Profile{}, err
	}
	if err := m.setSecret(id, value); err != nil {
		return Profile{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Profile{}, errors.Join(problem(503, "closed", "Environment manager is shutting down."), m.vault.Delete(vaultService, m.account+":"+id))
	}
	m.state.Profiles[id] = profile
	if err := m.saveLocked(); err != nil {
		delete(m.state.Profiles, id)
		return Profile{}, errors.Join(err, m.vault.Delete(vaultService, m.account+":"+id))
	}
	return profile, nil
}

func (m *Manager) target(id string) (Profile, *liveTarget, secret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.state.Profiles[id]
	if !ok {
		return p, nil, secret{}, problem(404, "target_not_found", "Unknown environment; no local fallback was used.")
	}
	if p.Revoked || m.closed {
		return p, nil, secret{}, problem(401, "revoked", "This environment is revoked or shutting down. Re-enroll explicitly.")
	}
	if target := m.live[id]; target != nil {
		return p, target, secret{}, nil
	}
	if p.Backend == "edge" {
		return p, nil, secret{}, problem(503, "offline", "Edge agent is offline. No mutation was queued; reconnect and review current state before retrying.")
	}
	value, err := m.getSecret(id)
	if err != nil {
		return p, nil, value, err
	}
	client, err := secureClient(p, value)
	if p.Backend == "kubernetes" {
		err = nil
	}
	if err != nil {
		return p, nil, value, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	target := &liveTarget{ctx: ctx, cancel: cancel}
	if client != nil {
		target.transport = client.Transport
		target.close = func() error { client.CloseIdleConnections(); return nil }
	}
	m.live[id] = target
	return p, target, value, nil
}

func (m *Manager) Proxy(w http.ResponseWriter, r *http.Request, id string) {
	p, target, _, err := m.target(id)
	if err != nil {
		WriteError(w, err)
		return
	}
	required, err := RequiredCapability(r.Method, r.URL.EscapedPath(), r.URL.Query())
	if err != nil {
		WriteError(w, err)
		return
	}
	if !slices.Contains(p.Capabilities, required) {
		WriteError(w, problem(403, "capability_denied", "The selected identity does not grant "+required+"."))
		return
	}
	if p.Backend == "kubernetes" {
		WriteError(w, problem(501, "kubernetes_adapter", "Use the imported environment's native Kubernetes administration routes."))
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(target.ctx, cancel)
	defer func() { stop(); cancel() }()
	r = r.Clone(ctx)
	if p.Backend == "docker" {
		m.dockerProxy(w, r, p, target)
		return
	}
	destination, err := endpoint(p.Address)
	if p.Backend == "edge" {
		destination, _ = url.Parse("http://edge.invalid")
		err = nil
	}
	if err != nil {
		WriteError(w, err)
		return
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = destination.Scheme, destination.Host
			pr.Out.URL.Path = "/v1/api" + pr.In.URL.Path
			if pr.In.URL.RawPath != "" {
				pr.Out.URL.RawPath = "/v1/api" + pr.In.URL.RawPath
			}
			pr.Out.Host = destination.Host
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Origin")
			pr.Out.Header.Del("Authorization")
			if p.Backend != "edge" {
				value, secretErr := m.getSecret(id)
				if secretErr != nil {
					cancel()
					return
				}
				pr.Out.Header.Set("Authorization", "Bearer "+value.Token)
			}
		},
		Transport:     target.transport,
		FlushInterval: -1,
		ModifyResponse: func(response *http.Response) error {
			if response.StatusCode >= 300 && response.StatusCode < 400 {
				return problem(502, "redirect_denied", "The selected target attempted a redirect. It was blocked; no request was redirected to this machine or another environment.")
			}
			response.Header.Del("Set-Cookie")
			response.Header.Set("X-Porto-Environment-ID", id)
			response.Header.Set("Cache-Control", "no-store")
			switch {
			case response.StatusCode == 401:
				m.mark(id, "unauthorized", "The enrolled credential is expired, revoked or rejected. Re-enroll explicitly.")
			case response.StatusCode == 403:
				m.mark(id, "restricted", "The selected server identity denied this resource or capability.")
			case response.StatusCode >= 500:
				m.mark(id, "offline", "The remote agent or its selected daemon is unavailable. No action was queued.")
			default:
				m.mark(id, "online", "")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			var known *Error
			if errors.As(err, &known) {
				m.mark(id, "error", known.Message)
				WriteError(w, known)
				return
			}
			m.mark(id, "offline", "Connection lost. Mutations are never automatically replayed.")
			WriteError(w, problem(503, "offline", "Selected environment is unreachable or its identity is invalid. No local fallback or queued retry was used; verify server trust, connectivity and enrollment."))
		},
	}
	proxy.ServeHTTP(w, r)
}

func (m *Manager) mark(id, state, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.state.Profiles[id]; ok {
		p.State, p.LastError = state, message
		if state == "online" || state == "restricted" || state == "unauthorized" {
			p.LastSeen = m.now().UTC()
		}

		m.state.Profiles[id] = p
	}
}

func (m *Manager) RecordHealth(id string, online bool) {
	if online {
		m.mark(id, "online", "")
	} else {
		m.mark(id, "offline", "Selected identity or endpoint is unavailable. No fallback or automatic action replay is used.")
	}
}

func (m *Manager) disconnectLocked(id string) error {
	target := m.live[id]
	delete(m.live, id)
	if target == nil {
		return nil
	}
	target.cancel()
	if target.close != nil {
		return target.close()
	}
	return nil
}

func (m *Manager) Rotate(ctx context.Context, id string) error {
	p, err := m.Profile(id)
	if err != nil {
		return err
	}
	if p.Backend != "porto" || p.Revoked {
		return problem(409, "rotation", "Only active Porto client identities support this rotation flow. Re-import external credentials or re-enroll Edge identities explicitly.")
	}
	value, err := m.getSecret(id)
	if err != nil {
		return err
	}
	client, err := secureClient(p, value)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	var result enrollmentResponse
	if err := exchange(ctx, client, p.Address+"/v1/identity/rotate", nil, &result); err != nil {
		return err
	}
	if result.Version != ProtocolVersion || result.Identity != p.Identity || result.Kind != "client" || len(result.Token) != 64 {
		return problem(502, "rotation_identity", "Rotation returned an incompatible identity. The credential was not adopted; verify this enrolled peer on the remote host.")
	}
	value.Token = result.Token
	m.mu.Lock()
	current, exists := m.state.Profiles[id]
	if !exists || current.Revoked || current.Identity != p.Identity || current.Address != p.Address || current.Certificate != p.Certificate {
		m.mu.Unlock()
		cleanupClient, cleanupErr := secureClient(p, value)
		if cleanupErr == nil {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			cleanupErr = exchange(cleanupCtx, cleanupClient, p.Address+"/v1/identity/revoke", nil, nil)
			cancel()
			cleanupClient.CloseIdleConnections()
		}
		return errors.Join(problem(409, "identity_changed", "The target was removed, revoked or changed during rotation. The new remote credential was not adopted; verify remote revocation."), cleanupErr)
	}
	defer m.mu.Unlock()
	p = current
	p.Revision++
	closeErr := m.disconnectLocked(id)
	if err := m.setSecret(id, value); err != nil {
		p.Revoked, p.State, p.LastError = true, "revoked", "Rotation succeeded remotely but the OS credential store failed. Re-enroll this target."
		m.state.Profiles[id] = p
		return errors.Join(err, closeErr, m.saveLocked())
	}
	m.state.Profiles[id] = p
	return errors.Join(closeErr, m.saveLocked())
}

func (m *Manager) Revoke(ctx context.Context, id string) error {
	m.mu.Lock()
	p, ok := m.state.Profiles[id]
	if !ok {
		m.mu.Unlock()
		return problem(404, "target_not_found", "Environment not found.")
	}
	p.Revoked, p.State = true, "revoked"
	p.Revision++
	m.state.Profiles[id] = p
	err := errors.Join(m.disconnectLocked(id), m.saveLocked())
	if p.Backend == "edge" {
		err = errors.Join(err, m.revokePeerLocked(p.Identity))
		p.RemoteRevoked = err == nil
	}
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if p.Backend == "porto" {
		value, err := m.getSecret(id)
		if err != nil {
			return err
		}
		client, err := secureClient(p, value)
		if err != nil {
			return err
		}
		defer client.CloseIdleConnections()
		if err := exchange(ctx, client, p.Address+"/v1/identity/revoke", nil, nil); err != nil {
			return problem(503, "revocation_pending", "Access is blocked locally, but remote revocation could not be confirmed. Restore connectivity and retry revocation, or revoke the client identity on the remote host.")
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, exists := m.state.Profiles[id]
	if !exists {
		return nil
	}
	current.RemoteRevoked = p.Backend == "porto" || p.Backend == "edge"
	m.state.Profiles[id] = current
	return m.saveLocked()
}

func (m *Manager) Remove(id string, localOnly bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.state.Profiles[id]
	if !ok {
		return problem(404, "target_not_found", "Environment not found.")
	}
	if !p.RemoteRevoked && p.Backend == "porto" && !localOnly {
		return problem(409, "revocation_pending", "Revoke this identity first, or explicitly confirm local-only removal while remote revocation is pending.")
	}
	p.Revoked, p.State = true, "revoked"
	m.state.Profiles[id] = p
	if err := errors.Join(m.disconnectLocked(id), m.saveLocked()); err != nil {
		return err
	}
	if p.Backend == "edge" {
		if err := m.revokePeerLocked(p.Identity); err != nil {
			return err
		}
		delete(m.state.Peers, p.Identity)
	} else if err := m.vault.Delete(vaultService, m.account+":"+id); err != nil {
		return fmt.Errorf("remove environment OS credential: %w", err)
	}
	delete(m.state.Profiles, id)
	return m.saveLocked()
}

func (m *Manager) Metadata(id string, request MetadataRequest) (Profile, error) {
	if err := validateMetadata(request.Name, request.Labels); err != nil {
		return Profile{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, exists := m.state.Profiles[id]
	if !exists {
		return Profile{}, problem(404, "target_not_found", "Enrolled environment not found.")
	}
	if current.Revision != request.Revision {
		return Profile{}, problem(409, "target_changed", "Environment changed while editing metadata; refresh and review its identity before retrying.")
	}
	previous := current
	current.Name, current.Labels = strings.TrimSpace(request.Name), cloneLabels(request.Labels)
	current.Revision++
	m.state.Profiles[id] = current
	if err := m.saveLocked(); err != nil {
		m.state.Profiles[id] = previous
		return Profile{}, err
	}
	return current, nil
}

func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	var errs []error
	for id := range m.live {
		errs = append(errs, m.disconnectLocked(id))
	}
	for _, peer := range m.peerContexts {
		peer.cancel()
	}
	listener := m.listener
	if m.listenerStop != nil {
		m.listenerStop()
		m.listenerStop = nil
	}
	m.mu.Unlock()
	if listener != nil {
		errs = append(errs, listener.Shutdown(ctx))
	}
	return errors.Join(errs...)
}
