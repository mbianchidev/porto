package docker

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestDockerAPICreatesStructuredBindVolumeAndTmpfsMounts(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		if len(command.Args) == 0 || command.Args[0] != "create" {
			return nil, fmt.Errorf("unexpected command: %+v", command)
		}
		joined := strings.Join(command.Args, " ")
		for _, expected := range []string{
			"--volume /host/source:/workspace:ro,rshared",
			"--volume app-data:/var/lib/app:nocopy",
			"--tmpfs /tmp:size=65536,mode=1777,noexec,nosuid",
		} {
			if !strings.Contains(joined, expected) {
				return nil, fmt.Errorf("missing %q in %s", expected, joined)
			}
		}
		return []byte("container-id\n"), nil
	}
	body := bytes.NewBufferString(`{
		"Image":"alpine:latest",
		"HostConfig":{
			"Mounts":[
				{"Type":"bind","Source":"/host/source","Target":"/workspace","ReadOnly":true,"BindOptions":{"Propagation":"rshared"}},
				{"Type":"volume","Source":"app-data","Target":"/var/lib/app","VolumeOptions":{"NoCopy":true}},
				{"Type":"tmpfs","Target":"/tmp","TmpfsOptions":{"SizeBytes":65536,"Mode":1023,"Options":[["noexec"],["nosuid"]]}}
			]
		}
	}`)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1.47/containers/create?name=demo", body)

	NewAPI(New(runner), "/tmp/porto.sock").ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", response.Code, response.Body.String())
	}
}

func TestDockerAPIRejectsUnsupportedStructuredMountBeforeRuntimeMutation(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	body := bytes.NewBufferString(`{
		"Image":"alpine:latest",
		"HostConfig":{
			"Mounts":[
				{"Type":"bind","Source":"/host/source","Target":"/workspace","BindOptions":{"NonRecursive":true}}
			]
		}
	}`)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1.47/containers/create", body)

	NewAPI(New(runner), "/tmp/porto.sock").ServeHTTP(response, request)

	if response.Code != http.StatusNotImplemented {
		t.Fatalf("create = %d: %s", response.Code, response.Body.String())
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.commands) != 0 {
		t.Fatalf("unsupported mount mutated runtime: %+v", runner.commands)
	}
}

func TestStructuredMountValidationRejectsDuplicateTargets(t *testing.T) {
	_, _, err := structuredMountArguments(
		[]dockerStructuredMount{
			{Type: "volume", Source: "one", Target: "/data"},
			{Type: "volume", Source: "two", Target: "/data"},
		},
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "duplicate mount target") {
		t.Fatalf("error = %v", err)
	}
}
