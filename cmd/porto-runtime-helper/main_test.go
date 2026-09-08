package main

import (
	"strings"
	"testing"
)

func TestParseCNIRequest(t *testing.T) {
	request, err := parseCNIRequest("cni-connect", []string{
		"--network", "backend",
		"--container", "demo",
		"--netns", "/proc/42/ns/net",
		"--aliases", "api",
	})
	if err != nil {
		t.Fatalf("parse request: %v", err)
	}
	if request.Network != "backend" || request.Container != "demo" ||
		request.NetNS != "/proc/42/ns/net" || len(request.Aliases) != 1 ||
		request.Aliases[0] != "api" {
		t.Fatalf("request = %+v", request)
	}
}

func TestParseCNIRequestRejectsMultipleAliases(t *testing.T) {
	_, err := parseCNIRequest("cni-connect", []string{
		"--network", "backend",
		"--container", "demo",
		"--netns", "/proc/42/ns/net",
		"--aliases", "api,internal",
	})
	if err == nil || !strings.Contains(err.Error(), "one DNS alias") {
		t.Fatalf("parse error = %v", err)
	}
}
