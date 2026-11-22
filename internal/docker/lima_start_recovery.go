package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Lima 2.3 tails ha.stdout.log without nxadm/tail's CompleteLines option, so
// `limactl start` can decode an event the host agent is still writing and exit
// while the host agent keeps booting the guest. A strict prefix of a JSON
// object always fails with this error.
const limaPartialEventFailure = "as events.Event: unexpected end of JSON input"

const limaHostAgentPollInterval = time.Second

type limaInstanceListing struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Dir    string `json:"dir"`
}

type limaHostAgentEvent struct {
	Time   time.Time `json:"time"`
	Status struct {
		Running  bool     `json:"running"`
		Degraded bool     `json:"degraded"`
		Exiting  bool     `json:"exiting"`
		Errors   []string `json:"errors"`
	} `json:"status"`
}

// limaEventLog reads only newline-terminated host-agent events, leaving a
// partially written final line for the next read.
type limaEventLog struct {
	path   string
	offset int64
}

func (l *limaEventLog) next() ([]limaHostAgentEvent, error) {
	file, err := os.Open(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open Lima host-agent events: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect Lima host-agent events: %w", err)
	}
	if info.Size() < l.offset {
		return nil, fmt.Errorf("Lima host-agent events in %s were truncated while Porto was reading them", l.path)
	}
	data, err := io.ReadAll(io.NewSectionReader(file, l.offset, info.Size()-l.offset))
	if err != nil {
		return nil, fmt.Errorf("read Lima host-agent events: %w", err)
	}
	end := bytes.LastIndexByte(data, '\n')
	if end < 0 {
		return nil, nil
	}
	var events []limaHostAgentEvent
	for _, line := range bytes.Split(data[:end], []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var event limaHostAgentEvent
		if err := json.Unmarshal(line, &event); err != nil {
			return nil, fmt.Errorf("decode Lima host-agent event in %s: %w", l.path, err)
		}
		events = append(events, event)
	}
	l.offset += int64(end + 1)
	return events, nil
}

func (m *Manager) listLimaInstance(ctx context.Context, action, instance string) (limaInstanceListing, error) {
	output, err := m.runStdoutCommand(ctx, 20*time.Second, action, "limactl", "list", instance, "--json")
	if err != nil {
		return limaInstanceListing{}, err
	}
	var listed limaInstanceListing
	if err := json.Unmarshal(output, &listed); err != nil || listed.Dir == "" || filepath.Base(listed.Dir) != instance {
		return limaInstanceListing{}, errors.New("Lima did not return the owned engine's configuration directory")
	}
	return listed, nil
}

// resumeLimaStart finishes a `limactl start` whose event watcher failed on a
// partially written event. It applies Lima's own readiness rules: a running
// event succeeds, a degraded or exiting event fails, and the host agent must
// stay alive within the original start deadline.
func (m *Manager) resumeLimaStart(ctx context.Context, instance string, begin time.Time) error {
	ctx, cancel := context.WithDeadline(ctx, begin.Add(engineStartTimeout))
	defer cancel()
	slog.Warn("Lima stopped on a partially written host-agent event; Porto is resuming the start wait", "instance", instance)
	if err := reportEngineInstall(ctx, "starting", "lima", "Waiting for the Lima guest and its provisioning checks."); err != nil {
		return err
	}
	var events *limaEventLog
	var hostAgentErrors []string
	for {
		listed, err := m.listLimaInstance(ctx, "inspect resumed Porto container runtime start", instance)
		if err != nil {
			return err
		}
		if events == nil {
			events = &limaEventLog{path: filepath.Join(listed.Dir, "ha.stdout.log")}
		}
		stderrPath := filepath.Join(listed.Dir, "ha.stderr.log")
		received, err := events.next()
		if err != nil {
			return err
		}
		for _, event := range received {
			if event.Time.Before(begin) {
				continue
			}
			if len(event.Status.Errors) > 0 {
				hostAgentErrors = event.Status.Errors
			}
			switch {
			case event.Status.Exiting:
				return fmt.Errorf("Lima host agent exited before the guest was ready%s (see %s)", limaHostAgentErrorSuffix(hostAgentErrors), stderrPath)
			case event.Status.Running && event.Status.Degraded:
				return fmt.Errorf("Lima reports the guest running but degraded%s (see %s)", limaHostAgentErrorSuffix(hostAgentErrors), stderrPath)
			case event.Status.Running:
				return nil
			}
		}
		// Liveness is read before events, so a final event written before exit is never missed.
		if !strings.EqualFold(listed.Status, "running") {
			return fmt.Errorf("Lima host agent stopped with status %q before the guest was ready%s (see %s)",
				listed.Status, limaHostAgentErrorSuffix(hostAgentErrors), stderrPath)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for the Lima guest to report running: %w", context.Cause(ctx))
		case <-time.After(limaHostAgentPollInterval):
		}
	}
}

func limaHostAgentErrorSuffix(messages []string) string {
	if len(messages) == 0 {
		return ""
	}
	return ": " + strings.Join(messages, "; ")
}
