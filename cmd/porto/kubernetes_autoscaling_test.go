package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/autoscaling"
	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/kubernetes"
)

type scalingRoundTripper func(*http.Request) (*http.Response, error)

func (f scalingRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestAutoscalingCLIPreservesOmittedSettings(t *testing.T) {
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	var sent autoscaling.Config
	http.DefaultTransport = scalingRoundTripper(func(request *http.Request) (*http.Response, error) {
		var value any
		switch {
		case request.URL.Path == "/api/health":
			value = map[string]any{"status": "ok", "apiVersion": config.APIVersion}
		case request.Method == http.MethodGet:
			value = autoscaling.Status{Config: autoscaling.Config{Nodes: autoscaling.NodeConfig{
				Enabled: true, MaxNodes: 2, Machine: kubernetes.MachineSpec{CPUs: 1, MemoryMiB: 1024, DiskGiB: 10},
			}}}
		default:
			if err := json.NewDecoder(request.Body).Decode(&sent); err != nil {
				t.Fatal(err)
			}
			value = sent
		}
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
	})
	if err := kubernetesAutoscalingCmd([]string{"fixture", "--keda"}); err != nil {
		t.Fatal(err)
	}
	if !sent.KEDA || !sent.Nodes.Enabled || sent.Nodes.MaxNodes != 2 || sent.Nodes.Machine.CPUs != 1 {
		t.Fatalf("omitted settings were overwritten: %+v", sent)
	}
}

func TestAutoscalingCLIUsageValidation(t *testing.T) {
	for _, run := range []func() error{
		func() error { return kubernetesAutoscalingCmd(nil) },
		func() error { return kubernetesWorkloadScalingCmd([]string{"fixture"}) },
		func() error { return kubernetesWorkloadUnscaleCmd([]string{"fixture", "default", "scaler"}) },
	} {
		if err := run(); err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Fatalf("usage error = %v", err)
		}
	}
}
