package process

import (
	"reflect"
	"testing"
)

func TestWithEnvironmentReplacesValues(t *testing.T) {
	got := WithEnvironment(
		[]string{"PATH=/bin", "DOCKER_HOST=unix:///old.sock", "PORT=3000"},
		"DOCKER_HOST=unix:///porto.sock",
		"DOCKER_CONTEXT=",
	)
	want := []string{
		"PATH=/bin",
		"PORT=3000",
		"DOCKER_HOST=unix:///porto.sock",
		"DOCKER_CONTEXT=",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environment = %#v, want %#v", got, want)
	}
}
