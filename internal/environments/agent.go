package environments

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"golang.org/x/net/netutil"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"time"
)

type EnrollmentRequest struct {
	Name         string   `json:"name"`
	Kind         string   `json:"kind"`
	Capabilities []string `json:"capabilities"`
	TTLSeconds   int      `json:"ttlSeconds"`
}
type Enrollment struct {
	Token       string    `json:"token"`
	ExpiresAt   time.Time `json:"expiresAt"`
	Address     string    `json:"address,omitempty"`
	Certificate string    `json:"certificate,omitempty"`
	Kind        string    `json:"kind"`
}
type invitation struct {
	Request   EnrollmentRequest `json:"request"`
	ExpiresAt time.Time         `json:"expiresAt"`
}
type peer struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Kind          string    `json:"kind"`
	TokenHash     string    `json:"tokenHash"`
	Capabilities  []string  `json:"capabilities"`
	ExpiresAt     time.Time `json:"expiresAt"`
	Revoked       bool      `json:"revoked"`
	EnvironmentID string    `json:"environmentId,omitempty"`
}
type PeerInfo struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Kind          string    `json:"kind"`
	Capabilities  []string  `json:"capabilities"`
	ExpiresAt     time.Time `json:"expiresAt"`
	Revoked       bool      `json:"revoked"`
	EnvironmentID string    `json:"environmentId,omitempty"`
}
type enrollmentResponse struct {
	Version       int       `json:"version"`
	Identity      string    `json:"identity"`
	Token         string    `json:"token"`
	Kind          string    `json:"kind"`
	Capabilities  []string  `json:"capabilities"`
	EnvironmentID string    `json:"environmentId,omitempty"`
	ExpiresAt     time.Time `json:"expiresAt"`
}
type ListenerOptions struct {
	Address       string `json:"address"`
	Hostname      string `json:"hostname"`
	PublicAddress string `json:"publicAddress"`
	Certificate   string `json:"certificate,omitempty"`
}

func tokenHash(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}
func newToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}
func CertificatePEM(certificate *x509.Certificate) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}))
}

type authTransport struct {
	base  *http.Transport
	token string
}

func (t *authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	copy := r.Clone(r.Context())
	copy.Header = r.Header.Clone()
	if t.token != "" {
		copy.Header.Set("Authorization", "Bearer "+t.token)
	}
	return t.base.RoundTrip(copy)
}
func (t *authTransport) CloseIdleConnections() { t.base.CloseIdleConnections() }

func secureClient(profile Profile, value secret) (*http.Client, error) {
	if _, err := endpoint(profile.Address); err != nil {
		return nil, err
	}
	block, rest := pem.Decode([]byte(profile.Certificate))
	if block == nil || block.Type != "CERTIFICATE" || strings.TrimSpace(string(rest)) != "" {
		return nil, problem(400, "server_identity", "Supply the exact server certificate obtained through a trusted channel. System trust or unverified TLS is not substituted.")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, problem(400, "server_identity", "The server certificate is invalid.")
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	config := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, VerifyConnection: func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 || !bytes.Equal(state.PeerCertificates[0].Raw, cert.Raw) {
			return errors.New("server certificate changed; verify and explicitly re-enroll")
		}
		return nil
	}}
	if value.ClientCertificate != "" || value.ClientKey != "" {
		clientCert, err := tls.X509KeyPair([]byte(value.ClientCertificate), []byte(value.ClientKey))
		if err != nil {
			return nil, problem(400, "client_identity", "The client certificate/key pair is invalid.")
		}
		config.Certificates = []tls.Certificate{clientCert}
	}
	transport := &http.Transport{TLSClientConfig: config, Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 15 * time.Second}).DialContext, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 30 * time.Second, MaxIdleConnsPerHost: 8, MaxConnsPerHost: 16, IdleConnTimeout: 30 * time.Second}
	return &http.Client{Transport: &authTransport{transport, value.Token}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("remote redirects are not authorized") }}, nil
}

func exchange(ctx context.Context, client *http.Client, address string, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, "POST", address, bytes.NewReader(data))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return problem(503, "connection", "TLS identity verification or connection failed. Check the pinned certificate, hostname, connectivity and enrollment; no insecure retry was attempted.")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var remote Error
		if json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&remote) == nil && remote.Code != "" && remote.Message != "" {
			remote.Status = response.StatusCode
			return &remote
		}
		return problem(response.StatusCode, "remote_rejected", "Remote agent rejected the request. Check authorization, expiry and protocol compatibility.")
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(out); err != nil {
			return problem(502, "protocol", "The agent returned an invalid response.")
		}
	}
	return nil
}

func (m *Manager) Enrollment(request EnrollmentRequest) (Enrollment, error) {
	if request.Kind != "client" && request.Kind != "edge" {
		return Enrollment{}, problem(400, "enrollment_kind", "Enrollment kind must be client or edge.")
	}
	if strings.TrimSpace(request.Name) == "" || len(request.Name) > 100 {
		return Enrollment{}, problem(400, "enrollment_name", "An enrollment name of 1-100 characters is required.")
	}
	if err := validateCapabilities(request.Capabilities); err != nil {
		return Enrollment{}, err
	}
	if request.TTLSeconds < 60 || request.TTLSeconds > 900 {
		return Enrollment{}, problem(400, "enrollment_expiry", "Enrollment expiry must be 60-900 seconds.")
	}
	token, err := newToken()
	if err != nil {
		return Enrollment{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Enrollment{}, problem(503, "agent_shutdown", "The secure agent is shutting down.")
	}
	for hash, value := range m.state.Invites {
		if !m.now().Before(value.ExpiresAt) {
			delete(m.state.Invites, hash)
		}
		for id, value := range m.state.Peers {
			if value.EnvironmentID == "" && (value.Revoked || !m.now().Before(value.ExpiresAt)) {
				delete(m.state.Peers, id)
				if scope := m.peerContexts[id]; scope != nil {
					scope.cancel()
					delete(m.peerContexts, id)
				}
			}
		}
	}
	if len(m.state.Invites) >= 128 || len(m.state.Peers) >= 512 {
		return Enrollment{}, problem(429, "enrollment_limit", "Enrollment limit reached. Revoke/remove unused identities before enrolling more.")
	}
	request.Capabilities = slices.Clone(request.Capabilities)
	expires := m.now().Add(time.Duration(request.TTLSeconds) * time.Second).UTC()
	m.state.Invites[tokenHash(token)] = invitation{request, expires}
	if err := m.saveLocked(); err != nil {
		delete(m.state.Invites, tokenHash(token))
		return Enrollment{}, err
	}
	result := Enrollment{Token: token, ExpiresAt: expires, Kind: request.Kind}
	if m.state.Listener != nil {
		result.Address, result.Certificate = m.state.Listener.PublicAddress, m.state.Listener.Certificate
	}
	return result, nil
}

func (m *Manager) enroll(token, kind string) (enrollmentResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return enrollmentResponse{}, problem(503, "agent_shutdown", "The secure agent is shutting down.")
	}
	hash := tokenHash(token)
	invite, ok := m.state.Invites[hash]
	if !ok || !m.now().Before(invite.ExpiresAt) {
		return enrollmentResponse{}, problem(401, "enrollment_invalid", "Enrollment is invalid, expired or already used. Issue a new short-lived enrollment on the trusted host.")
	}
	if kind != invite.Request.Kind {
		return enrollmentResponse{}, problem(403, "enrollment_kind", "This single-use enrollment belongs to a different identity kind. It was not consumed; use the intended client or outbound Edge flow.")
	}
	id, err := ID()
	if err != nil {
		return enrollmentResponse{}, err
	}
	credential, err := newToken()
	if err != nil {
		return enrollmentResponse{}, err
	}
	current := peer{ID: id, Name: invite.Request.Name, Kind: invite.Request.Kind, TokenHash: tokenHash(credential), Capabilities: invite.Request.Capabilities, ExpiresAt: m.now().Add(30 * 24 * time.Hour).UTC()}
	if current.Kind == "edge" {
		envID, err := ID()
		if err != nil {
			return enrollmentResponse{}, err
		}
		current.EnvironmentID = envID
		m.state.Profiles[envID] = Profile{ID: envID, Name: current.Name, Backend: "edge", Address: "outbound", Identity: id, Capabilities: slices.Clone(current.Capabilities), Labels: map[string]string{}, Revision: 1, State: "offline"}
	}
	delete(m.state.Invites, hash)
	m.state.Peers[id] = current
	if err := m.saveLocked(); err != nil {
		m.state.Invites[hash] = invite
		delete(m.state.Peers, id)
		delete(m.state.Profiles, current.EnvironmentID)
		return enrollmentResponse{}, err
	}
	return enrollmentResponse{ProtocolVersion, id, credential, current.Kind, current.Capabilities, current.EnvironmentID, current.ExpiresAt}, nil
}

func (m *Manager) authenticate(r *http.Request) (peer, *liveTarget, error) {
	value := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if len(value) != 64 || r.Header.Get("Authorization") == value {
		return peer{}, nil, problem(401, "unauthorized", "A valid, unexpired enrolled identity is required.")
	}
	hash := tokenHash(value)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return peer{}, nil, problem(503, "agent_shutdown", "The secure agent is shutting down.")
	}
	for _, p := range m.state.Peers {
		if subtle.ConstantTimeCompare([]byte(hash), []byte(p.TokenHash)) != 1 || p.Revoked || !m.now().Before(p.ExpiresAt) {
			continue
		}
		scope := m.peerContexts[p.ID]
		if scope == nil {
			ctx, cancel := context.WithDeadline(context.Background(), p.ExpiresAt)
			scope = &liveTarget{ctx: ctx, cancel: cancel}
			m.peerContexts[p.ID] = scope
		}
		return p, scope, nil
	}
	return peer{}, nil, problem(401, "unauthorized", "Identity is invalid, expired or revoked. Re-enroll explicitly.")
}

func (m *Manager) Peers() []PeerInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	values := []PeerInfo{}
	for _, p := range m.state.Peers {
		values = append(values, PeerInfo{p.ID, p.Name, p.Kind, slices.Clone(p.Capabilities), p.ExpiresAt, p.Revoked, p.EnvironmentID})
	}
	slices.SortFunc(values, func(a, b PeerInfo) int { return strings.Compare(a.ID, b.ID) })
	return values
}

func (m *Manager) revokePeerLocked(id string) error {
	p, ok := m.state.Peers[id]
	if !ok {
		return problem(404, "identity_not_found", "Enrolled identity not found.")
	}
	p.Revoked = true
	m.state.Peers[id] = p
	if target := m.peerContexts[id]; target != nil {
		target.cancel()
		delete(m.peerContexts, id)
	}
	if env, exists := m.state.Profiles[p.EnvironmentID]; p.EnvironmentID != "" && exists {
		env.Revoked, env.RemoteRevoked, env.State = true, true, "revoked"
		env.Revision++
		m.state.Profiles[env.ID] = env
		if err := m.disconnectLocked(env.ID); err != nil {
			return err
		}
	}
	return m.saveLocked()
}
func (m *Manager) RevokePeer(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.revokePeerLocked(id)
}

func (m *Manager) AgentHandler(local http.Handler) http.Handler {
	slots := make(chan struct{}, 32)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Header.Get("Origin") != "" {
			WriteError(w, problem(403, "origin", "The agent network listener does not accept browser-origin requests."))
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			WriteError(w, problem(429, "busy", "Agent concurrency limit reached."))
			return
		}
		if r.Method == "POST" && r.URL.Path == "/v1/enroll" {
			var request struct {
				Token string `json:"token"`
				Kind  string `json:"kind"`
			}
			if !Decode(w, r, &request) {
				return
			}
			value, err := m.enroll(request.Token, request.Kind)
			if err != nil {
				WriteError(w, err)
				return
			}
			JSON(w, 201, value)
			return
		}
		p, scope, err := m.authenticate(r)
		if err != nil {
			WriteError(w, err)
			return
		}
		if r.Method == "POST" && r.URL.Path == "/v1/identity/revoke" {
			if err := m.RevokePeer(p.ID); err != nil {
				WriteError(w, err)
				return
			}
			JSON(w, 200, map[string]string{"status": "revoked"})
			return
		}
		if r.Method == "POST" && r.URL.Path == "/v1/identity/rotate" {
			token, err := newToken()
			if err != nil {
				WriteError(w, err)
				return
			}
			m.mu.Lock()
			current := m.state.Peers[p.ID]
			if current.TokenHash != p.TokenHash || current.Revoked || m.closed || !m.now().Before(current.ExpiresAt) {
				m.mu.Unlock()
				WriteError(w, problem(409, "identity_changed", "Identity changed concurrently. Re-enroll if necessary."))
				return
			}
			current.TokenHash, current.ExpiresAt = tokenHash(token), m.now().Add(30*24*time.Hour).UTC()
			m.state.Peers[p.ID] = current
			err = m.saveLocked()
			if err != nil {
				m.state.Peers[p.ID] = p
			} else {
				scope.cancel()
				delete(m.peerContexts, p.ID)
			}
			m.mu.Unlock()
			if err != nil {
				WriteError(w, err)
				return
			}
			JSON(w, 200, enrollmentResponse{ProtocolVersion, p.ID, token, p.Kind, p.Capabilities, p.EnvironmentID, current.ExpiresAt})
			return
		}
		if r.Method == "GET" && r.URL.Path == "/v1/edge/connect" {
			if p.Kind != "edge" {
				WriteError(w, problem(403, "identity_kind", "Only an explicitly enrolled Edge agent can open an outbound connection."))
				return
			}
			m.acceptEdge(w, r, p, scope)
			return
		}
		if p.Kind != "client" || !strings.HasPrefix(r.URL.Path, "/v1/api/api/") {
			WriteError(w, problem(403, "agent_route", "This identity cannot use the requested agent route."))
			return
		}
		targetPath := strings.TrimPrefix(r.URL.Path, "/v1/api")
		required, err := RequiredCapability(r.Method, strings.TrimPrefix(r.URL.EscapedPath(), "/v1/api"), r.URL.Query())
		if err != nil {
			WriteError(w, err)
			return
		}
		if !slices.Contains(p.Capabilities, required) {
			WriteError(w, problem(403, "capability_denied", "Identity does not grant "+required+"."))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
		stop := context.AfterFunc(scope.ctx, cancel)
		defer func() { stop(); cancel() }()
		copy := r.Clone(ctx)
		copy.URL.Path = targetPath
		if copy.URL.RawPath != "" {
			copy.URL.RawPath = strings.TrimPrefix(copy.URL.RawPath, "/v1/api")
		}
		copy.Host = "127.0.0.1:37623"
		copy.Header.Del("Authorization")
		copy.Header.Del("Cookie")
		copy.Header.Del("Origin")
		copy.Body = http.MaxBytesReader(w, copy.Body, MaxBody)
		local.ServeHTTP(w, copy)
	})
}

func LoopbackHandler(address string) (http.Handler, error) {
	value, err := url.Parse(address)
	if err != nil || value.Scheme != "http" || value.User != nil || value.Path != "" || value.RawQuery != "" || value.Fragment != "" {
		return nil, errors.New("agent daemon address must be an explicit loopback HTTP origin")
	}
	host := net.ParseIP(value.Hostname())
	if host == nil || !host.IsLoopback() {
		return nil, errors.New("agent daemon must use a loopback IP, never a network host")
	}
	proxy := httputil.NewSingleHostReverseProxy(value)
	proxy.Transport = &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext}
	director := proxy.Director
	proxy.Director = func(r *http.Request) { director(r); r.Host = value.Host }
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		WriteError(w, problem(503, "local_daemon_offline", "The agent's loopback Porto daemon is unavailable. Start it on the enrolled host; no other daemon was used."))
	}
	return proxy, nil
}

func (m *Manager) StartListener(ctx context.Context, options ListenerOptions, local http.Handler) (ListenerOptions, error) {
	if options.Address == "" || options.Hostname == "" {
		return options, problem(400, "listener", "An explicit listen address and certificate hostname are required.")
	}
	if _, _, err := net.SplitHostPort(options.Address); err != nil {
		return options, problem(400, "listener", "Listen address must include host and port.")
	}
	public, err := endpoint(options.PublicAddress)
	if err != nil {
		return options, err
	}
	if public.Hostname() != options.Hostname {
		return options, problem(400, "listener_hostname", "The public HTTPS address must match the certificate hostname.")
	}
	options.PublicAddress = public.String()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listener != nil || m.closed {
		return options, problem(409, "listener_active", "The secure listener is already active or shutting down. Restart to change its explicit address.")
	}
	var certificate tls.Certificate
	if m.state.Listener != nil {
		if m.state.Listener.Hostname != options.Hostname {
			return options, problem(409, "identity_change", "Certificate hostname changes require explicit listener identity replacement and re-enrollment, not automatic trust changes.")
		}
		options.Certificate = m.state.Listener.Certificate
		private, err := m.getSecret("listener")
		if err != nil {
			return options, err
		}
		certificate, err = tls.X509KeyPair([]byte(options.Certificate), []byte(private.ClientKey))
		if err != nil {
			return options, fmt.Errorf("read listener TLS identity: %w", err)
		}
	} else {
		var keyPEM string
		options.Certificate, keyPEM, err = generateIdentity(options.Hostname, m.now())
		if err != nil {
			return options, err
		}
		if err := m.setSecret("listener", secret{ClientKey: keyPEM}); err != nil {
			return options, err
		}
		certificate, err = tls.X509KeyPair([]byte(options.Certificate), []byte(keyPEM))
		if err != nil {
			return options, err
		}
	}
	listener, err := net.Listen("tcp", options.Address)
	if err != nil {
		return options, fmt.Errorf("start explicitly requested secure listener: %w", err)
	}
	previous := m.state.Listener
	m.state.Listener = &options
	if err := m.saveLocked(); err != nil {
		m.state.Listener = previous
		return options, errors.Join(err, listener.Close())
	}
	server := &http.Server{Handler: m.AgentHandler(local), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 32 << 10, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}, NextProtos: []string{"http/1.1"}}}
	m.listener, m.local = server, local
	go func() {
		if err := server.Serve(tls.NewListener(netutil.LimitListener(listener, 64), server.TLSConfig)); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("Secure environment listener stopped", "error", err)
		}
	}()
	m.listenerStop = context.AfterFunc(ctx, func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(closeCtx); err != nil {
			slog.Warn("Close secure environment listener", "error", err)
		}
	})
	return options, nil
}

func (m *Manager) Listener() *ListenerOptions {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.Listener == nil {
		return nil
	}
	copy := *m.state.Listener
	return &copy
}

func (m *Manager) StopListener(ctx context.Context) error {
	m.mu.Lock()
	server := m.listener
	if m.listenerStop != nil {
		m.listenerStop()
		m.listenerStop = nil
	}
	m.state.Listener = nil
	for id, p := range m.state.Peers {
		p.Revoked = true
		m.state.Peers[id] = p
		if scope := m.peerContexts[id]; scope != nil {
			scope.cancel()
			delete(m.peerContexts, id)
		}
		if env, exists := m.state.Profiles[p.EnvironmentID]; p.EnvironmentID != "" && exists {
			env.Revoked, env.RemoteRevoked, env.State = true, true, "revoked"
			env.Revision++
			m.state.Profiles[env.ID] = env
			if err := m.disconnectLocked(env.ID); err != nil {
				m.mu.Unlock()
				return err
			}
		}
	}
	m.state.Invites = map[string]invitation{}
	err := m.saveLocked()
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if server != nil {
		if err := server.Shutdown(ctx); err != nil {
			return err
		}
	}
	m.mu.Lock()
	m.listener = nil
	m.mu.Unlock()
	if err := m.vault.Delete(vaultService, m.account+":listener"); err != nil {
		return fmt.Errorf("remove secure listener OS credential: %w", err)
	}
	return nil
}

func generateIdentity(host string, now time.Time) (string, string, error) {
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", "", err
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Porto enrolled agent"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(365 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &private.PublicKey, private)
	if err != nil {
		return "", "", err
	}
	key, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})), nil
}
