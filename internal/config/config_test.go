package config

import (
	"os"
	"strings"
	"testing"
)

func TestProjectHTTPSURLUsesConfiguredRouterPort(t *testing.T) {
	t.Setenv("PORTO_HOME", t.TempDir())
	t.Setenv(RouterTLSAddrEnv, "127.0.0.1:37681")
	if got := ProjectHTTPSURL("devoidofbeauty.com"); got != "https://devoidofbeauty.com.porto.localhost:37681/" {
		t.Fatalf("URL = %q", got)
	}

	t.Setenv(RouterTLSAddrEnv, "127.0.0.1:443")
	if got := ProjectHTTPSURL("devoidofbeauty.com"); got != "https://devoidofbeauty.com.porto.localhost/" {
		t.Fatalf("portless URL = %q", got)
	}

	t.Setenv(RouterTLSAddrEnv, "127.0.0.1:37681")
	t.Setenv(RouterTLSPublicPortEnv, "443")
	if got := ProjectHTTPSURL("devoidofbeauty.com"); got != "https://devoidofbeauty.com.porto.localhost/" {
		t.Fatalf("forwarded portless URL = %q", got)
	}
}

func TestProjectHTTPSURLUsesPortlessMarker(t *testing.T) {
	t.Setenv("PORTO_HOME", t.TempDir())
	t.Setenv(RouterTLSAddrEnv, "127.0.0.1:37681")
	t.Setenv(RouterTLSPublicPortEnv, "")
	markerPath, err := PortlessHTTPSMarkerPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ProjectHTTPSURL("app"); got != "https://app.porto.localhost/" {
		t.Fatalf("URL = %q", got)
	}
}

func TestProjectHostnameCompactsBranch(t *testing.T) {
	if got := ProjectHostname("2dnd", "copilot/improve-elemental-resistances-system", "main"); got != "2dnd-cop-imp-ele-res-sys" {
		t.Fatalf("hostname = %q", got)
	}
	if got := ProjectHostname("2dnd", "main", "main"); got != "2dnd" {
		t.Fatalf("default hostname = %q", got)
	}
}

func TestProjectHostnameRespectsDNSLabelLimit(t *testing.T) {
	for _, base := range []string{strings.Repeat("a", 50), strings.Repeat("a", 63)} {
		got := ProjectHostname(base, strings.Repeat("long-branch-name-", 20), "main")
		if len(got) > 63 {
			t.Fatalf("hostname label length = %d, want <= 63: %q", len(got), got)
		}
	}
}
