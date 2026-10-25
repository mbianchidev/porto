package docker

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/mbianchidev/porto/internal/wsl"
	"github.com/mbianchidev/porto/internal/wslbridge"
)

func TestDockerAPITranslatesSelectedWSLBindsAndPreservesExplicitWatchSettings(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		args := strings.Join(command.Args, "\n")
		for _, expected := range []string{
			"/mnt/porto-wsl/synthetic/project's caf\u00e9:/work:ro",
			"data:/data", "/mnt/porto-wsl/synthetic/other:/other",
			"CHOKIDAR_USEPOLLING=true", "WATCHPACK_POLLING=250",
		} {
			if !strings.Contains(args, expected) {
				return nil, fmt.Errorf("missing %q in %q", expected, command.Args)
			}
		}
		return []byte("synthetic-container\n"), nil
	}
	manager := New(runner)
	manager.wslResolve = func(context.Context, string, string) (wsl.Integration, error) {
		return wsl.Integration{Shares: []wsl.Share{{LinuxPath: "/home/synthetic", GuestPath: "/mnt/porto-wsl/synthetic"}}}, nil
	}
	request := httptest.NewRequest(http.MethodPost, "/v1.47/containers/create", bytes.NewBufferString(`{
		"Image":"alpine","Env":["WATCHPACK_POLLING=250"],
		"HostConfig":{"Binds":["/home/synthetic/project's caf\u00e9:/work:ro","data:/data"],
		"Mounts":[{"Type":"bind","Source":"/home/synthetic/other","Target":"/other"}]}
	}`))
	request.Header.Set(wslbridge.DistributionHeader, "synthetic")
	request.Header.Set(wslbridge.OwnerHeader, "synthetic-owner")
	response := httptest.NewRecorder()
	NewAPI(manager, "/synthetic/docker.sock").ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("selected WSL create = %d: %s", response.Code, response.Body.String())
	}
}

func TestDockerAPIRejectsUnapprovedWSLIdentityBeforeBackendAccess(t *testing.T) {
	api := NewAPI(New(nil), "/synthetic/docker.sock")
	request := httptest.NewRequest(http.MethodGet, "/_ping", nil)
	request.Header.Set(wslbridge.DistributionHeader, "non-opted-in")
	request.Header.Set(wslbridge.OwnerHeader, "foreign")
	response := httptest.NewRecorder()
	api.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("unapproved WSL identity received HTTP %d", response.Code)
	}
}
