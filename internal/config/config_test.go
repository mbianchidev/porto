package config

import (
	"os"
	"testing"
)

func TestProjectHTTPSURLUsesConfiguredRouterPort(t *testing.T) {
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
