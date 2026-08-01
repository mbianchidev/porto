package config

import "testing"

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
