package docker

import (
	"errors"
	"testing"
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
