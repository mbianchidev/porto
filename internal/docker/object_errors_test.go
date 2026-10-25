package docker

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/containerd/errdefs"
	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestMissingDockerObjectsRecognizesNativeStoreDiagnosticsOnly(t *testing.T) {
	for _, test := range []struct {
		kind, message string
		missing       bool
	}{
		{"volume", `volume-store error\nnot found\n"synthetic/volume.json" does not exist`, true},
		{"network", `no network found matching: synthetic-network`, true},
		{"volume", `volume-store error\npermission denied\n"synthetic/volume.json"`, false},
		{"volume", `volume-store error\nnot found\npermission denied`, false},
		{"image", `image store "synthetic/volume.json" does not exist`, false},
	} {
		if actual := missingDockerObject(errors.New(test.message), test.kind); actual != test.missing {
			t.Errorf("%s: missing=%v, expected %v", test.message, actual, test.missing)
		}
	}
}

func TestMissingDockerObjectsDoNotTreatAbsentContentAsAbsentImage(t *testing.T) {
	err := fmt.Errorf("resolve pinned source OCI configuration: %w", errdefs.ErrNotFound)
	if missingDockerObject(err, "image") {
		t.Fatal("missing content in an existing image was mistaken for permission to replace it")
	}
}

func TestDockerNetworkInspectionClassifiesNativeMissingObjects(t *testing.T) {
	for _, test := range []struct {
		name, diagnostic string
		status           int
	}{
		{"missing", "no network found matching: synthetic-network\nunable to find any network matching the provided request", http.StatusNotFound},
		{"permission", "network store: permission denied", http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{handler: func(runtimes.Command) ([]byte, error) {
				return nil, errors.New(test.diagnostic)
			}}
			response := httptest.NewRecorder()
			NewAPI(New(runner), "").ServeHTTP(response,
				httptest.NewRequest(http.MethodGet, "/v1.47/networks/synthetic-network", nil))
			if response.Code != test.status {
				t.Fatalf("network inspection = %d: %s; want %d", response.Code, response.Body.String(), test.status)
			}
		})
	}
}
