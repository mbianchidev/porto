package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/app"
	portodocker "github.com/mbianchidev/porto/internal/docker"
	"github.com/mbianchidev/porto/internal/registries"
	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/mbianchidev/porto/internal/store"
	"github.com/zalando/go-keyring"
)

func TestRegistryHandlersStoreSecretInKeyringAndVerifyPull(t *testing.T) {
	keyring.MockInit()
	st, err := store.Open(filepath.Join(t.TempDir(), "porto.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var pulledConfig []byte
	runner := runtimeRunnerFunc(func(_ context.Context, command runtimes.Command) ([]byte, error) {
		if command.Name != "nerdctl" || !strings.Contains(strings.Join(command.Args, " "), "pull ghcr.io/example/private:latest") {
			return nil, nil
		}
		for _, entry := range command.Env {
			if configDir, ok := strings.CutPrefix(entry, "DOCKER_CONFIG="); ok {
				pulledConfig, err = os.ReadFile(filepath.Join(configDir, "config.json"))
				return nil, err
			}
		}
		t.Fatal("verified pull did not receive a scoped Docker config")
		return nil, nil
	})
	manager := portodocker.New(runner)
	server := &Server{
		store:         st,
		docker:        manager,
		registryVault: registries.NewVault(),
	}
	manager.SetRegistryAuthResolver(server.registryAuthForImage)

	createResponse := httptest.NewRecorder()
	server.createRegistry(
		createResponse,
		httptest.NewRequest(http.MethodPost, "/api/registries", bytes.NewBufferString(`{
			"name":"GitHub packages",
			"provider":"github",
			"server":"ghcr.io",
			"username":"octocat",
			"credential":"test-token",
			"testImage":"ghcr.io/example/private:latest",
			"enabled":true
		}`)),
	)
	if createResponse.Code != http.StatusCreated {
		t.Fatalf("create registry = %d: %s", createResponse.Code, createResponse.Body.String())
	}
	var created app.RegistryProfile
	if err := json.Unmarshal(createResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(createResponse.Body.String(), "test-token") || !created.CredentialStored || created.Verified {
		t.Fatalf("unsafe or invalid registry response: %s", createResponse.Body.String())
	}

	verifyRequest := httptest.NewRequest(http.MethodPost, "/api/registries/1/verify", nil)
	verifyRequest.SetPathValue("id", "1")
	verifyResponse := httptest.NewRecorder()
	server.verifyRegistry(verifyResponse, verifyRequest)
	if verifyResponse.Code != http.StatusOK {
		t.Fatalf("verify registry = %d: %s", verifyResponse.Code, verifyResponse.Body.String())
	}
	if !strings.Contains(string(pulledConfig), `"ghcr.io"`) ||
		!strings.Contains(string(pulledConfig), `"auth":"b2N0b2NhdDp0ZXN0LXRva2Vu"`) {
		t.Fatalf("pull config = %s", pulledConfig)
	}
	verified, err := st.Registry(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !verified.Verified || verified.LastVerifiedAt == "" {
		t.Fatalf("verified profile = %+v", verified)
	}
}

func TestRegistryHandlerRejectsTestImageFromDifferentServer(t *testing.T) {
	keyring.MockInit()
	st, err := store.Open(filepath.Join(t.TempDir(), "porto.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	server := &Server{
		store:         st,
		docker:        portodocker.New(runtimeRunnerFunc(func(context.Context, runtimes.Command) ([]byte, error) { return nil, nil })),
		registryVault: registries.NewVault(),
	}
	response := httptest.NewRecorder()
	server.createRegistry(
		response,
		httptest.NewRequest(http.MethodPost, "/api/registries", bytes.NewBufferString(`{
			"name":"GitHub packages",
			"provider":"github",
			"server":"ghcr.io",
			"username":"octocat",
			"credential":"test-token",
			"testImage":"alpine:latest",
			"enabled":true
		}`)),
	)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "belongs to") {
		t.Fatalf("mismatched test image response = %d: %s", response.Code, response.Body.String())
	}
}

func TestNormalizeSettingsDefaultsAndValidatesExperience(t *testing.T) {
	normalized, err := normalizeSettings(app.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	if normalized.InterfaceDensity != app.DefaultInterfaceDensity ||
		normalized.TerminalFontSize != app.DefaultTerminalFontSize ||
		normalized.TerminalLineHeight != app.DefaultTerminalLineHeight ||
		normalized.TerminalScrollback != app.DefaultTerminalScrollback {
		t.Fatalf("normalized settings = %+v", normalized)
	}
	normalized.TerminalFontSize = 25
	if _, err := normalizeSettings(normalized); err == nil || !strings.Contains(err.Error(), "font size") {
		t.Fatalf("invalid font size error = %v", err)
	}
}

func TestRegistryVerificationRejectsStaleProfileResult(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "porto.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	profile, err := st.CreateRegistry(context.Background(), app.RegistryProfile{
		Name:             "GitHub",
		Provider:         "github",
		Server:           "ghcr.io",
		Username:         "octocat",
		TestImage:        "ghcr.io/example/private:latest",
		Enabled:          true,
		CredentialStored: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	updated := profile
	updated.Name = "Changed while verifying"
	if err := st.UpdateRegistry(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: st}
	if _, err := server.finishRegistryVerification(context.Background(), profile, nil); !errors.Is(err, errRegistryProfileChanged) {
		t.Fatalf("stale verification error = %v", err)
	}
	current, err := st.Registry(context.Background(), profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Verified {
		t.Fatal("stale verification result changed the profile")
	}
}
