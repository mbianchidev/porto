package environments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
	"github.com/mbianchidev/porto/internal/config"
)

func muxConfig() *yamux.Config {
	config := yamux.DefaultConfig()
	config.AcceptBacklog = 16
	config.KeepAliveInterval = 10 * time.Second
	config.ConnectionWriteTimeout = 10 * time.Second
	config.StreamOpenTimeout = 10 * time.Second
	config.StreamCloseTimeout = 5 * time.Second
	config.MaxStreamWindowSize = 256 << 10
	return config
}

func (m *Manager) acceptEdge(w http.ResponseWriter, r *http.Request, p peer, scope *liveTarget) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		slog.Warn("Accept enrolled Edge connection", "identity", p.ID, "error", err)
		return
	}
	defer conn.CloseNow()
	ctx, cancel := context.WithCancel(scope.ctx)
	defer cancel()
	session, err := yamux.Client(websocket.NetConn(ctx, conn, websocket.MessageBinary), muxConfig())
	if err != nil {
		slog.Warn("Initialize Edge tunnel", "identity", p.ID, "error", err)
		return
	}
	defer session.Close()
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if session.NumStreams() >= 16 {
			return nil, errors.New("Edge stream limit reached")
		}
		return session.Open()
	}, MaxConnsPerHost: 16, MaxIdleConnsPerHost: 8, ResponseHeaderTimeout: 30 * time.Second, IdleConnTimeout: 15 * time.Second}
	target := &liveTarget{ctx: ctx, cancel: cancel, transport: transport, close: func() error { transport.CloseIdleConnections(); return session.Close() }}
	m.mu.Lock()
	current := m.state.Peers[p.ID]
	if current.Revoked || current.TokenHash != p.TokenHash || m.closed {
		m.mu.Unlock()
		return
	}
	if err := m.disconnectLocked(p.EnvironmentID); err != nil {
		slog.Warn("Replace old Edge connection", "identity", p.ID, "error", err)
	}
	m.live[p.EnvironmentID] = target
	profile := m.state.Profiles[p.EnvironmentID]
	profile.Revision++
	profile.State, profile.LastSeen, profile.LastError = "online", m.now().UTC(), ""
	m.state.Profiles[profile.ID] = profile
	err = m.saveLocked()
	m.mu.Unlock()
	if err != nil {
		slog.Error("Persist Edge connection identity", "error", err)
		return
	}
	defer func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.live[p.EnvironmentID] == target {
			delete(m.live, p.EnvironmentID)
			profile := m.state.Profiles[p.EnvironmentID]
			profile.State, profile.LastError = "offline", "Outbound agent disconnected. Last inventory is stale; actions are not queued or replayed."
			m.state.Profiles[profile.ID] = profile
		}
	}()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-session.CloseChan():
			return
		case <-ticker.C:
			if _, err := session.Ping(); err != nil {
				return
			}
			m.mark(p.EnvironmentID, "online", "")
		}
	}
}

type edgeConnection struct {
	Address       string    `json:"address"`
	Certificate   string    `json:"certificate"`
	Identity      string    `json:"identity"`
	EnvironmentID string    `json:"environmentId"`
	Capabilities  []string  `json:"capabilities"`
	ExpiresAt     time.Time `json:"expiresAt"`
}

// RunEdge is explicitly invoked on the enrolled host. It opens no network listener.
func (m *Manager) RunEdge(ctx context.Context, invite *Enrollment, local http.Handler) error {
	var connection edgeConnection
	if invite != nil {
		client, err := secureClient(Profile{Address: invite.Address, Certificate: invite.Certificate}, secret{})
		if err != nil {
			return err
		}
		var result enrollmentResponse
		err = exchange(ctx, client, invite.Address+"/v1/enroll", map[string]string{"token": invite.Token, "kind": "edge"}, &result)
		client.CloseIdleConnections()
		if err != nil {
			return err
		}
		if result.Version != ProtocolVersion || result.Kind != "edge" || !safeID.MatchString(result.EnvironmentID) {
			return problem(502, "edge_enrollment", "The endpoint did not return a compatible, explicitly enrolled Edge identity.")
		}
		connection = edgeConnection{invite.Address, invite.Certificate, result.Identity, result.EnvironmentID, result.Capabilities, result.ExpiresAt}
		if err := m.setSecret("edge-client", secret{Token: result.Token}); err != nil {
			return err
		}
		data, err := json.Marshal(connection)
		if err != nil {
			return err
		}
		if err := config.WriteFileAtomic(filepath.Join(m.root, "edge-client.json"), data, 0o600); err != nil {
			return err
		}
	} else {
		data, err := os.ReadFile(filepath.Join(m.root, "edge-client.json"))
		if err != nil {
			return fmt.Errorf("load explicit Edge enrollment; pass a new enrollment file on first connection: %w", err)
		}
		if err := json.Unmarshal(data, &connection); err != nil {
			return fmt.Errorf("read Edge enrollment: %w", err)
		}
	}
	value, err := m.getSecret("edge-client")
	if err != nil {
		return err
	}
	if err := validateCapabilities(connection.Capabilities); err != nil {
		return err
	}
	client, err := secureClient(Profile{Address: connection.Address, Certificate: connection.Certificate}, value)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithDeadline(ctx, connection.ExpiresAt)
	defer cancel()
	delay := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := runEdgeConnection(ctx, client, connection, local)
		if ctx.Err() != nil {
			break
		}
		var known *Error
		if errors.As(err, &known) && (known.Status == 401 || known.Status == 403) {
			return err
		}
		slog.Warn("Edge connection interrupted; bounded reconnect scheduled", "identity", connection.Identity, "retryAfter", delay, "error", err)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
		if time.Since(start) > 30*time.Second {
			delay = time.Second
		} else {
			delay = min(delay*2, 30*time.Second)
		}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return problem(401, "edge_expired", "Edge identity expired. Revoke the old identity and explicitly enroll a replacement.")
	}
	return ctx.Err()
}

func runEdgeConnection(ctx context.Context, client *http.Client, connection edgeConnection, local http.Handler) error {
	address := "wss" + strings.TrimPrefix(connection.Address, "https") + "/v1/edge/connect"
	conn, response, err := websocket.Dial(ctx, address, &websocket.DialOptions{HTTPClient: client, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		if response != nil {
			if response.Body != nil {
				response.Body.Close()
			}
			if response.StatusCode == 401 || response.StatusCode == 403 {
				return problem(response.StatusCode, "edge_revoked", "The manager rejected this Edge identity. Re-enroll explicitly; reconnects will not bypass revocation.")
			}
		}
		return fmt.Errorf("connect enrolled Edge tunnel: %w", err)
	}
	defer conn.CloseNow()
	session, err := yamux.Server(websocket.NetConn(ctx, conn, websocket.MessageBinary), muxConfig())
	if err != nil {
		return err
	}
	defer session.Close()
	slots := make(chan struct{}, 16)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			WriteError(w, problem(429, "busy", "Edge execution limit reached."))
			return
		}
		targetPath := strings.TrimPrefix(r.URL.Path, "/v1/api")
		if !strings.HasPrefix(r.URL.Path, "/v1/api/api/") {
			WriteError(w, problem(403, "edge_route", "Only the enrolled API tunnel is available."))
			return
		}
		required, err := RequiredCapability(r.Method, strings.TrimPrefix(r.URL.EscapedPath(), "/v1/api"), r.URL.Query())
		if err != nil {
			WriteError(w, err)
			return
		}
		if !slices.Contains(connection.Capabilities, required) {
			WriteError(w, problem(403, "capability_denied", "Edge enrollment does not grant "+required+"."))
			return
		}
		requestCtx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
		defer cancel()
		copy := r.Clone(requestCtx)
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
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 32 << 10}
	stop := context.AfterFunc(ctx, func() { session.Close() })
	defer stop()
	slog.Info("Edge connection authenticated", "identity", connection.Identity, "environment", connection.EnvironmentID)
	err = server.Serve(session)
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return errors.Join(err, server.Shutdown(closeCtx))
}

func (m *Manager) ForgetEdge(ctx context.Context, localOnly bool) error {
	data, err := os.ReadFile(filepath.Join(m.root, "edge-client.json"))
	if err != nil {
		return err
	}
	var connection edgeConnection
	if err := json.Unmarshal(data, &connection); err != nil {
		return err
	}
	if !localOnly {
		value, err := m.getSecret("edge-client")
		if err != nil {
			return err
		}
		client, err := secureClient(Profile{Address: connection.Address, Certificate: connection.Certificate}, value)
		if err != nil {
			return err
		}
		err = exchange(ctx, client, connection.Address+"/v1/identity/revoke", nil, nil)
		client.CloseIdleConnections()
		if err != nil {
			return problem(503, "edge_revocation_pending", "Manager revocation could not be confirmed. Revoke the Edge identity on its manager, then explicitly use --local-only to forget this host's credential.")
		}
	}
	if err := m.vault.Delete(vaultService, m.account+":edge-client"); err != nil {
		return fmt.Errorf("remove Edge credential from OS store: %w", err)
	}
	if err := os.Remove(filepath.Join(m.root, "edge-client.json")); err != nil {
		return err
	}
	if localOnly {
		slog.Warn("Edge enrollment forgotten locally; manager-side revocation must be verified independently", "identity", connection.Identity)
	}
	return nil
}
