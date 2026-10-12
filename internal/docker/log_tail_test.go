package docker

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestDockerLogTailAcceptsStandardAllLinesSentinel(t *testing.T) {
	runner := &fakeRunner{
		handler: func(runtimes.Command) ([]byte, error) {
			return []byte(`[{"Id":"synthetic","Config":{"Tty":false}}]`), nil
		},
		streamer: func(command runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
			if !strings.Contains(strings.Join(command.Args, " "), "--tail all") {
				t.Fatalf("standard all-lines sentinel was not preserved: %v", command.Args)
			}
			return nil, emit(runtimes.OutputChunk{Stream: "stdout", Data: []byte("synthetic log\n")})
		},
	}
	response := httptest.NewRecorder()
	NewAPI(New(runner), "/tmp/porto.sock").ServeHTTP(response,
		httptest.NewRequest(http.MethodGet, "/v1.47/containers/synthetic/logs?stdout=true&stderr=true&follow=true&tail=-1", nil))
	if response.Code != http.StatusOK || !response.Flushed || !bytes.Contains(response.Body.Bytes(), []byte("synthetic log")) {
		t.Fatalf("Dockerode log response = %d: %q", response.Code, response.Body.Bytes())
	}
	if tail, err := parseLogTail("-1"); err != nil || tail != allLogLines {
		t.Fatalf("backend all-lines sentinel = %d, error=%v", tail, err)
	}
	if _, err := parseLogTail("-2"); err == nil {
		t.Fatal("invalid negative log tail was accepted")
	}
}
