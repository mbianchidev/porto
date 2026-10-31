package environments

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestRemoteTerminalUpgradeClosesOnRevocation(t *testing.T) {
	manager, agent := testManager(t), testManager(t)
	serverClosed := make(chan struct{})
	remote := httptest.NewTLSServer(agent.AgentHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		if err := conn.Write(r.Context(), websocket.MessageBinary, []byte("synthetic remote terminal")); err != nil {
			return
		}
		_, _, _ = conn.Read(r.Context())
		close(serverClosed)
	})))
	defer remote.Close()
	invite, err := agent.Enrollment(EnrollmentRequest{Name: "synthetic-terminal", Kind: "client", Capabilities: []string{InventoryRead, TerminalOpen}, TTLSeconds: 120})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := manager.Add(context.Background(), AddRequest{Name: "synthetic-terminal", Backend: "porto", Address: remote.URL, Certificate: CertificatePEM(remote.Certificate()), EnrollmentToken: invite.Token})
	if err != nil {
		t.Fatal(err)
	}
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { manager.Proxy(w, r, profile.ID) }))
	defer local.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(local.URL, "http")+"/api/docker/containers/same-name/terminal", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	_, message, err := conn.Read(ctx)
	if err != nil || string(message) != "synthetic remote terminal" {
		t.Fatalf("wrong terminal target: %q %v", message, err)
	}
	if err := manager.Revoke(ctx, profile.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.Read(ctx); err == nil {
		t.Fatal("revoked terminal remained open")
	}
	select {
	case <-serverClosed:
	case <-time.After(3 * time.Second):
		t.Fatal("remote terminal handle survived revocation")
	}
}
