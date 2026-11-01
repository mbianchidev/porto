package environments

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRemoteRedirectCannotRetargetBrowserActionsToLocalAPI(t *testing.T) {
	manager, agent := testManager(t), testManager(t)
	remote := httptest.NewTLSServer(agent.AgentHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/api/projects/same-name/start", http.StatusTemporaryRedirect)
	})))
	defer remote.Close()
	profile := enrollHost(t, manager, agent, remote, "synthetic-redirect")
	response := httptest.NewRecorder()
	manager.Proxy(response, httptest.NewRequest("POST", "/api/docker/containers/same-name/stop", nil), profile.ID)
	if response.Code != 502 || response.Header().Get("Location") != "" {
		t.Fatalf("redirect exposed an unscoped local API: %d %v", response.Code, response.Header())
	}
}

func TestEnrollmentKindMismatchDoesNotConsumeOrCreateAnEdgeTarget(t *testing.T) {
	manager := testManager(t)
	invite, err := manager.Enrollment(EnrollmentRequest{Name: "edge-only", Kind: "edge", Capabilities: []string{InventoryRead}, TTLSeconds: 120})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := manager.enroll(invite.Token, "client"); err == nil {
		t.Fatal("client consumed an outbound Edge enrollment")
	}
	if len(manager.List()) != 1 {
		t.Fatal("wrong-kind attempt created a target")
	}
	if _, err := manager.enroll(invite.Token, "edge"); err != nil {
		t.Fatalf("wrong-kind attempt consumed enrollment: %v", err)
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.enroll(invite.Token, "edge"); err == nil {
		t.Fatal("closed agent created a new identity")
	}
}

func TestSlashedImageInspectionCannotBypassSensitiveInspectionGrant(t *testing.T) {
	for _, path := range []string{"/api/docker/images/sha256:synthetic", "/api/docker/images/registry.invalid/synthetic/image:one", "/api/docker/images/logs", "/api/docker/images/registry.invalid%2Flogs%2Fterminal%2Fimage:one"} {
		required, err := RequiredCapability("GET", path, nil)
		if err != nil || required != SecretsRead {
			t.Fatalf("sensitive image route %s: %s %v", path, required, err)
		}
	}
	if _, err := RequiredCapability("POST", "/api/docker/images/registry.invalid/synthetic:one/analysis", nil); err == nil {
		t.Fatal("remote scan started without exposing its diagnostic/result lifecycle")
	}
}
