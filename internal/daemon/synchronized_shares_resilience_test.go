package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	portodocker "github.com/mbianchidev/porto/internal/docker"
	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/mbianchidev/porto/internal/syncshares"
)

func ownedShareScript(t *testing.T, ctx context.Context, manager *portodocker.Manager, script string, arguments ...string) []byte {
	t.Helper()
	ownership := manager.EngineOwnershipStatus(ctx)
	if !ownership.Owned || !ownership.Verified {
		t.Fatalf("refusing a guest filesystem operation without current engine ownership: %+v", ownership)
	}
	args := append([]string{"shell", "--workdir=/", "porto-engine", "--", "sh", "-c", script, "porto-share-acceptance"}, arguments...)
	output, err := (runtimes.ExecRunner{}).Run(ctx, runtimes.Command{Name: "limactl", Args: args})
	if err != nil {
		t.Fatalf("operate only on the selected synthetic guest share: %v: %s", err, output)
	}
	return output
}

func testOwnedShareReconnectAndCaseCollisions(t *testing.T, ctx context.Context, server *Server, manager *portodocker.Manager, share syncshares.Share) {
	t.Helper()
	waitConnected := func(requireReady bool) {
		t.Helper()
		waitPolicy(t, ctx, func() bool {
			status, err := server.syncShares.Status(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(status.Shares) != 1 || status.Shares[0].Session == nil {
				return false
			}
			current := status.Shares[0]
			return current.Session.Alpha.Connected && current.Session.Beta.Connected &&
				current.Session.Alpha.Scanned && current.Session.Beta.Scanned && (!requireReady || current.Error == "")
		}, "same owned synchronization session reconnects and scans both endpoints")
	}
	ownedShareScript(t, ctx, manager, `set -eu; printf %s synthetic-guest-version > "$1/guest-before-disconnect.txt"`, share.GuestPath)
	if _, err := server.syncShares.Resolve(ctx, share.HostPath); err != nil {
		t.Fatal(err)
	}
	if err := manager.StopEngine(ctx); err != nil {
		t.Fatal(err)
	}
	status, err := server.syncShares.Status(ctx)
	if err != nil || len(status.Shares) != 1 || status.Shares[0].Error == "" {
		t.Fatalf("disconnected guest was advertised as a ready synchronized share: %+v, %v", status, err)
	}
	if _, err := server.syncShares.Resolve(ctx, share.HostPath); err == nil {
		t.Fatal("a disconnected share silently fell back to ordinary or stale data")
	}
	if err := os.WriteFile(filepath.Join(share.HostPath, "host-offline.txt"), []byte("synthetic-offline-host-version"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.StartEngine(ctx); err != nil {
		t.Fatal(err)
	}
	waitConnected(true)
	if mapped, err := server.syncShares.Resolve(ctx, share.HostPath); err != nil || mapped != share.GuestPath {
		t.Fatalf("original share did not reconnect to its identity-bound guest directory: %q, %v", mapped, err)
	}
	status, err = server.syncShares.Status(ctx)
	if err != nil || status.Shares[0].Session == nil || status.Shares[0].Session.Identifier != share.SessionID ||
		status.Shares[0].OwnerID != share.OwnerID || status.Shares[0].GuestIdentity != share.GuestIdentity {
		t.Fatalf("reconnect adopted a replacement endpoint or session: %+v, %v", status, err)
	}
	data, err := os.ReadFile(filepath.Join(share.HostPath, "guest-before-disconnect.txt"))
	if err != nil || string(data) != "synthetic-guest-version" {
		t.Fatalf("reconnect lost the guest's pre-disconnect edit: %q, %v", data, err)
	}
	output := ownedShareScript(t, ctx, manager, `set -eu; cat "$1/host-offline.txt"`, share.GuestPath)
	if strings.TrimSpace(string(output)) != "synthetic-offline-host-version" {
		t.Fatalf("reconnect lost a host edit made while offline: %q", output)
	}
	if err := server.syncShares.SetMode(ctx, share.ID, "bind"); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(share.HostPath, "SyntheticCaseProbe")
	if err := os.WriteFile(probe, []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, caseErr := os.Stat(filepath.Join(share.HostPath, "syntheticcaseprobe"))
	caseInsensitive := caseErr == nil
	if caseErr != nil && !errors.Is(caseErr, os.ErrNotExist) {
		t.Fatal(caseErr)
	}
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}
	ownedShareScript(t, ctx, manager, `set -eu
printf %s synthetic-uppercase > "$1/SyntheticCase.txt"
printf %s synthetic-lowercase > "$1/syntheticcase.txt"
`, share.GuestPath)
	if err := server.syncShares.SetMode(ctx, share.ID, "synchronized"); err != nil {
		t.Fatal(err)
	}
	waitConnected(false)
	if caseInsensitive {
		waitPolicy(t, ctx, func() bool {
			status, err := server.syncShares.Status(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(status.Shares) != 1 || status.Shares[0].Session == nil {
				return false
			}
			current := status.Shares[0]
			problems := len(current.Session.Alpha.ScanProblems) + len(current.Session.Alpha.TransitionProblems) +
				len(current.Session.Beta.ScanProblems) + len(current.Session.Beta.TransitionProblems)
			return current.Error != "" && problems > 0
		}, "a real case-insensitive host reports guest case collisions")
		if _, err := server.syncShares.Resolve(ctx, share.HostPath); err == nil {
			t.Fatal("a colliding share was exposed as a ready mount")
		}
	} else {
		if _, err := server.syncShares.Resolve(ctx, share.HostPath); err != nil {
			t.Fatal(err)
		}
		for name, expected := range map[string]string{"SyntheticCase.txt": "synthetic-uppercase", "syntheticcase.txt": "synthetic-lowercase"} {
			data, err := os.ReadFile(filepath.Join(share.HostPath, name))
			if err != nil || string(data) != expected {
				t.Fatalf("a case-sensitive host lost a distinct filename: %s: %q: %v", name, data, err)
			}
		}
	}
	output = ownedShareScript(t, ctx, manager, `set -eu; cat "$1/SyntheticCase.txt"; printf '\n'; cat "$1/syntheticcase.txt"`, share.GuestPath)
	if strings.TrimSpace(string(output)) != "synthetic-uppercase\nsynthetic-lowercase" {
		t.Fatalf("case collision discarded a guest version: %q", output)
	}
	if err := server.syncShares.SetMode(ctx, share.ID, "bind"); err != nil {
		t.Fatal(err)
	}
	ownedShareScript(t, ctx, manager, `set -eu; rm -- "$1/SyntheticCase.txt" "$1/syntheticcase.txt"`, share.GuestPath)
	for _, name := range []string{"SyntheticCase.txt", "syntheticcase.txt"} {
		if err := os.Remove(filepath.Join(share.HostPath, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	if err := server.syncShares.SetMode(ctx, share.ID, "synchronized"); err != nil {
		t.Fatal(err)
	}
	waitConnected(true)
	if _, err := server.syncShares.Resolve(ctx, share.HostPath); err != nil {
		t.Fatal(err)
	}
	t.Logf("real guest stop/start, offline host edit, retained guest edit, same-session reconnect, and case-collision preservation passed; host case-insensitive=%t", caseInsensitive)
}
