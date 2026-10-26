package daemon

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	portodocker "github.com/mbianchidev/porto/internal/docker"
	"github.com/mbianchidev/porto/internal/runtimes"
)

func testOwnedGuestSynchronizedShare(t *testing.T, ctx context.Context, server *Server, manager *portodocker.Manager, image, architecture string) {
	t.Helper()
	root := t.TempDir()
	for index := range 10000 {
		directory := filepath.Join(root, fmt.Sprintf("package-%03d", index/100))
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, fmt.Sprintf("fixture-%05d.txt", index)), []byte("synthetic repository data\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "host-note.txt"), []byte("synthetic selected host root"), 0o600); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	share, err := server.syncShares.Create(ctx, 1, root, []string{"node_modules"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("real owned guest synchronization: 10,000 entries ready in %s; only selected root=%s", time.Since(started), share.ID)
	testSynchronizedShareHostKeyPin(t, ctx, server, share.RemoteHost)
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := reservation.Addr().(*net.TCPAddr).Port
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	id, err := manager.RunContainer(ctx, portodocker.CreateContainerRequest{
		Name: fmt.Sprintf("porto-sync-policy-%d", time.Now().UnixNano()), Image: image, Platform: "linux/" + architecture,
		Publish: []string{fmt.Sprintf("127.0.0.1:%d:8080/tcp", port)}, Volumes: []string{root + ":/workspace:rw"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := manager.ContainerAction(cleanupContext, id, "remove-force"); err != nil {
			t.Error(err)
		}
	})
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	waitPolicy(t, ctx, func() bool {
		response, err := client.Get(url + "/read")
		if err != nil {
			return false
		}
		data, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		return readErr == nil && closeErr == nil && response.StatusCode == http.StatusOK && string(data) == "synthetic selected host root"
	}, "actual mapped container reads its selected synchronized root")
	response, err := client.Post(url+"/write", "text/plain", bytes.NewBufferString("synthetic guest-side edit"))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		t.Fatalf("actual guest write failed: %s", data)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	waitPolicy(t, ctx, func() bool {
		data, err := os.ReadFile(filepath.Join(root, "guest-note.txt"))
		return err == nil && string(data) == "synthetic guest-side edit"
	}, "bidirectional guest change reaches only the selected host root")
	watch, err := client.Get(url + "/watch")
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Body.Close()
	reader := bufio.NewReader(watch.Body)
	if line, err := reader.ReadString('\n'); err != nil || line != "watching\n" {
		t.Fatalf("guest filesystem notification watch was not installed: %q, %v", line, err)
	}
	if err := os.WriteFile(filepath.Join(root, "host-notification.txt"), []byte("synthetic notification event"), 0o600); err != nil {
		t.Fatal(err)
	}
	if line, err := reader.ReadString('\n'); err != nil || line != "notified\n" {
		t.Fatalf("a host edit did not produce a real container inotify event: %q, %v", line, err)
	}
	t.Log("real container bind mapping, host/guest edits, and container inotify propagation passed through the owned maintained-tool session")
}

func testSynchronizedShareHostKeyPin(t *testing.T, ctx context.Context, server *Server, host string) {
	t.Helper()
	directory := filepath.Join(server.store.Directory(), "sync-ssh")
	wrapper := filepath.Join(directory, "ssh")
	runner := runtimes.ExecRunner{}
	command := runtimes.Command{Name: wrapper, Args: []string{"-T", host, "printf", "synthetic-pinned-ssh"}}
	output, err := runner.Run(ctx, command)
	if err != nil || !strings.Contains(string(output), "synthetic-pinned-ssh") {
		t.Fatalf("verified private SSH endpoint was not usable: %v: %s", err, output)
	}
	key := filepath.Join(t.TempDir(), "synthetic-unrelated-key")
	if output, err := runner.Run(ctx, runtimes.Command{Name: "ssh-keygen", Args: []string{"-q", "-t", "ed25519", "-N", "", "-f", key}}); err != nil {
		t.Fatalf("generate only a synthetic host-key mismatch: %v: %s", err, output)
	}
	publicKey, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	knownHosts := filepath.Join(directory, "known_hosts")
	original, err := os.ReadFile(knownHosts)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(original)), "\n")
	for index, line := range lines {
		host, _, _ := strings.Cut(line, " ")
		lines[index] = host + " " + strings.TrimSpace(string(publicKey))
	}
	defer func() {
		if err := os.WriteFile(knownHosts, original, 0o600); err != nil {
			t.Error(err)
		}
	}()
	if err := os.WriteFile(knownHosts, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err = runner.Run(ctx, command)
	if err == nil || !strings.Contains(string(output), "Host key verification failed") {
		t.Fatalf("private SSH ignored a mismatched pinned guest key: %v: %s", err, output)
	}
}
