package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/etcqlite"
)

func TestEtcqliteCLIPinsFullUint64IdentityAndExplicitSelection(t *testing.T) {
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	var requests []struct{ method, path, body string }
	http.DefaultTransport = scalingRoundTripper(func(request *http.Request) (*http.Response, error) {
		var value any = map[string]string{"message": "synthetic"}
		switch request.URL.Path {
		case "/api/health":
			value = map[string]any{"status": "ok", "apiVersion": config.APIVersion}
		case "/api/integrations/etcqlite":
			value = etcqlite.Status{Supported: true, State: "configured", Instances: []etcqlite.Instance{{Name: "synthetic-db", ClusterID: "18446744073709551001"}}}
		default:
			body, _ := io.ReadAll(request.Body)
			requests = append(requests, struct{ method, path, body string }{request.Method, request.URL.Path, string(body)})
		}
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
	})
	if err := etcqliteCmd([]string{"delete", "synthetic-db", "--confirm"}); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || requests[0].method != "DELETE" || !strings.Contains(requests[0].body, `"clusterID":"18446744073709551001"`) {
		t.Fatalf("CLI lost or failed to pin identity: %+v", requests)
	}
	if err := kubernetesClusterCmd([]string{"create", "synthetic-k3s", "--etcqlite", "synthetic-db", "--workers", "0"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(requests[1].body, `"etcqlite":{"name":"synthetic-db","clusterID":"18446744073709551001"}`) {
		t.Fatalf("explicit k3s selector was not identity-bound: %s", requests[1].body)
	}
}

func TestEtcqliteCLIRejectsUnconfirmedOrUnsupportedOperationsLocally(t *testing.T) {
	for _, args := range [][]string{
		{"delete", "synthetic"}, {"restore", "synthetic", "--confirm"}, {"create", "synthetic", "--members", "2"},
		{"logs", "synthetic", "--stream", "foreign"}, {"logs", "synthetic", "--tail", "1001"},
	} {
		if err := etcqliteCmd(args); err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Fatalf("unsafe CLI request %v accepted: %v", args, err)
		}
	}
	if err := kubernetesClusterCmd([]string{"create", "synthetic", "--provider", "k0s", "--etcqlite", "synthetic-db"}); err == nil || !strings.Contains(err.Error(), "only") {
		t.Fatalf("unrelated distribution claimed the datastore: %v", err)
	}
}
