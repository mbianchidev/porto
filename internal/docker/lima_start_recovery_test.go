package docker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/porto-containers/porto/internal/runtimes"
)

func TestLimaEventLogWaitsForPartiallyWrittenEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ha.stdout.log")
	complete := `{"time":"2026-11-22T11:01:58Z","status":{"sshLocalPort":32835}}` + "\n"
	partial := `{"time":"2026-11-22T11:01:59Z","status":{"running":true}}` + "\n"
	if err := os.WriteFile(path, []byte(complete+partial[:30]), 0o600); err != nil {
		t.Fatal(err)
	}
	events := &limaEventLog{path: path}
	received, err := events.next()
	if err != nil || len(received) != 1 || received[0].Status.Running {
		t.Fatalf("first read = %+v, %v; want only the complete event", received, err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(partial[30:]); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	received, err = events.next()
	if err != nil || len(received) != 1 || !received[0].Status.Running {
		t.Fatalf("second read = %+v, %v; want the completed running event", received, err)
	}
}

func TestStartLimaRuntimeResumesAfterLimaReadsPartialEvent(t *testing.T) {
	t.Setenv("LIMA_HOME", t.TempDir())
	const watcherFailure = "level=fatal msg=\"failed to unmarshal `{\\\"time\\\":\\\"2026-11-22T11:01:58Z\\\",\\\"status\\\":{` as events.Event: unexpected end of JSON input\""
	event := func(status string) string {
		return `{"time":"` + time.Now().UTC().Format(time.RFC3339Nano) + `","status":` + status + "}\n"
	}
	tests := []struct {
		name        string
		status      string
		final       string
		startOutput string
		want        string
	}{
		{name: "running", status: "Running", final: `{"running":true}`},
		{name: "degraded", status: "Running", final: `{"running":true,"degraded":true}`, want: "running but degraded"},
		{name: "exiting", status: "Stopped", final: `{"exiting":true,"errors":["guest agent did not start"]}`, want: "guest agent did not start"},
		// The pre-start running event proves stale events cannot satisfy readiness.
		{name: "stopped", status: "Stopped", want: `stopped with status "Stopped"`},
		{name: "unrelated failure", status: "Running", final: `{"running":true}`, startOutput: "qemu-system-aarch64: cannot allocate memory", want: "cannot allocate memory"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), engineInstanceName)
			if err := os.Mkdir(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			eventsPath := filepath.Join(directory, "ha.stdout.log")
			listing, err := json.Marshal(limaInstanceListing{Name: engineInstanceName, Status: test.status, Dir: directory})
			if err != nil {
				t.Fatal(err)
			}
			var remainder string
			listed := 0
			runner := &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
				switch strings.Join(command.Args, " ") {
				case "start --timeout=20m0s porto-engine":
					forwarded := event(`{"portForward":{"type":"forwarding","protocol":"udp","guestAddr":"[::]:5353","hostAddr":"127.0.0.1:5353"}}`)
					remainder = forwarded[len(forwarded)/2:]
					stale := `{"time":"2000-01-01T00:00:00Z","status":{"running":true}}` + "\n"
					if err := os.WriteFile(eventsPath, []byte(stale+forwarded[:len(forwarded)/2]), 0o600); err != nil {
						return nil, err
					}
					output := watcherFailure
					if test.startOutput != "" {
						output = test.startOutput
					}
					return []byte(output), errors.New("exit status 1")
				case "list porto-engine --json":
					listed++
					if listed == 1 {
						appended := remainder
						if test.final != "" {
							appended += event(test.final)
						}
						file, err := os.OpenFile(eventsPath, os.O_APPEND|os.O_WRONLY, 0)
						if err != nil {
							return nil, err
						}
						_, err = file.WriteString(appended)
						if closeErr := file.Close(); err == nil {
							err = closeErr
						}
						if err != nil {
							return nil, err
						}
					}
					return listing, nil
				}
				return nil, errors.New("unexpected command: " + strings.Join(command.Args, " "))
			}}

			_, err = New(runner).startLimaRuntime(context.Background(), "start Porto container runtime", engineInstanceName)
			if test.want == "" {
				if err != nil {
					t.Fatalf("resumed start failed: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("start error = %v, want %q", err, test.want)
			}
			if test.startOutput != "" {
				if listed != 0 {
					t.Fatalf("unrelated start failure resumed the wait with %d Lima inspections", listed)
				}
			} else if !strings.Contains(err.Error(), limaPartialEventFailure) {
				t.Fatalf("resumed failure dropped Lima's original error: %v", err)
			}
		})
	}
}
