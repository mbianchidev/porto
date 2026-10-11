package docker

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestDockerExecSupportsOrdinaryHTTPStreamingClients(t *testing.T) {
	runner := &fakeRunner{
		handler: func(runtimes.Command) ([]byte, error) {
			return []byte(`[{"Id":"synthetic","State":{"Running":true},"Config":{"Tty":false}}]`), nil
		},
		starter: func(runtimes.Command) (runtimes.Process, error) {
			return newFakeProcess(func([]byte) ([]byte, []byte, error) {
				return []byte("synthetic HTTP exec\n"), nil, nil
			}), nil
		},
	}
	server := httptest.NewServer(NewAPI(New(runner), "/tmp/porto.sock"))
	defer server.Close()
	client := server.Client()
	client.Timeout = 3 * time.Second
	created, err := client.Post(server.URL+"/v1.47/containers/synthetic/exec", "application/json",
		strings.NewReader(`{"Cmd":["sh","-c","true"],"AttachStdout":true,"AttachStderr":true}`))
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		ID string `json:"Id"`
	}
	if err := json.NewDecoder(created.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	_ = created.Body.Close()
	response, err := client.Post(server.URL+"/v1.47/exec/"+result.ID+"/start", "application/json",
		strings.NewReader(`{"Detach":false,"Tty":false}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	output, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK || !bytes.Contains(output, []byte("synthetic HTTP exec")) {
		t.Fatalf("ordinary HTTP exec stream = %d: %q, error=%v", response.StatusCode, output, err)
	}
	if len(output) < 8 || output[0] != 1 {
		t.Fatal("ordinary HTTP exec lost Docker multiplex framing")
	}
}
