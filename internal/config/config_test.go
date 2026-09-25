package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLogPathUsesPlatformStateDirectoryAndPortoHome(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("PORTO_HOME", "")
	t.Setenv("HOME", directory)
	t.Setenv("APPDATA", filepath.Join(directory, "Roaming"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(directory, "xdg"))
	base := filepath.Join(directory, "xdg")
	switch runtime.GOOS {
	case "darwin":
		base = filepath.Join(directory, "Library", "Application Support")
	case "windows":
		base = filepath.Join(directory, "Roaming")
	}
	got, err := LogPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(base, "porto", "logs", "porto.log"); got != want {
		t.Fatalf("platform log path = %q, want %q", got, want)
	}
	custom := filepath.Join(directory, "synthetic portable")
	t.Setenv("PORTO_HOME", custom)
	got, err = LogPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(custom, "logs", "porto.log"); got != want {
		t.Fatalf("custom log path = %q, want %q", got, want)
	}
}

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
