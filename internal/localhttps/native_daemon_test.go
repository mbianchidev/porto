//go:build linux || windows || darwin

package localhttps

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/certificates"
	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/store"
)

func TestNativePortlessHTTPSRealDaemonRestartAndRenewal(t *testing.T) {
	if os.Getenv("PORTO_TEST_NATIVE_HTTPS") != "1" {
		t.Skip("requires the disposable native HTTPS acceptance row")
	}
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		t.Fatal("real helper/daemon acceptance is restricted to disposable CI hosts")
	}
	binary := os.Getenv("PORTO_TEST_HTTPS_BINARY")
	if binary == "" {
		t.Fatal("current built Porto CLI is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	home, err := os.MkdirTemp(os.Getenv("RUNNER_TEMP"), "porto-https-daemon-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(home); err != nil {
			t.Errorf("remove only owned daemon fixture: %v", err)
		}
	})
	certificate := filepath.Join(home, "certificates", "porto.local.pem")
	manager := certificates.New(certificate, filepath.Join(home, "certificates", "porto.local-key.pem"),
		filepath.Join(home, "certificates", "porto-root-ca.pem"), filepath.Join(home, "certificates", "porto-root-ca-key.pem"))
	if _, err := manager.Ensure(); err != nil {
		t.Fatal(err)
	}
	if Snapshot(certificate).Installed {
		t.Fatal("refusing to replace an existing HTTPS integration")
	}
	db, err := store.Open(filepath.Join(home, "porto.db"))
	if err != nil {
		t.Fatal(err)
	}
	settings, err := db.Settings(ctx)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	settings.DockerEnabled, settings.KubernetesEnabled, settings.VMsEnabled = false, false, false
	if err := errors.Join(db.SetSettings(ctx, settings), db.Close()); err != nil {
		t.Fatal(err)
	}
	ui := filepath.Join(home, "dashboard")
	if err := os.Mkdir(ui, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ui, "index.html"), []byte("<html><body>porto-real-daemon-ready</body></html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{config.DaemonAddr, config.RouterAddr, config.RouterTLSAddr} {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			t.Fatalf("refuse occupied daemon fixture endpoint %s: %v", address, err)
		}
		listener.Close()
	}
	if output, err := runNativeHTTPSCommand(t, ctx, binary, home, "https", "install"); err != nil {
		t.Fatalf("install real daemon HTTPS fixture: %v %s", err, output)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		for _, args := range [][]string{{"https", "uninstall"}, {"cert", "untrust"}} {
			if output, err := runNativeHTTPSCommand(t, cleanup, binary, home, args...); err != nil {
				t.Errorf("remove owned daemon HTTPS fixture: %v %s", err, output)
			}
		}
	})
	if err := prepareNativeDaemonOwner(home); err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: config.LocalhostDomain},
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", ListenAddress)
		}}
	defer transport.CloseIdleConnections()
	trusted := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	read := func() (string, error) {
		response, err := trusted.Get("https://" + config.LocalhostDomain + "/")
		if err != nil {
			return "", err
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(data), "porto-real-daemon-ready") {
			return "", errors.New("actual daemon did not serve its synthetic dashboard through trusted HTTPS")
		}
		return string(response.TLS.PeerCertificates[0].Raw), nil
	}
	var process *exec.Cmd
	var exited chan error
	stopProcess := func() {
		if process == nil {
			return
		}
		if err := process.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("terminate exact owned daemon fixture: %v", err)
		}
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			t.Error("owned daemon did not exit")
		}
		process = nil
	}
	t.Cleanup(stopProcess)
	for attempt := 0; attempt < 2; attempt++ {
		process, err = nativeUnprivilegedDaemon(t, ctx, binary, home, ui)
		if err != nil {
			t.Fatal(err)
		}
		log, err := os.OpenFile(filepath.Join(home, "daemon.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		process.Stdout, process.Stderr = log, log
		if err := startNativeDaemon(t, ctx, process); err != nil {
			log.Close()
			t.Fatal(err)
		}
		exited = make(chan error, 1)
		go func(command *exec.Cmd) { exited <- command.Wait(); log.Close() }(process)
		deadline := time.Now().Add(20 * time.Second)
		for {
			if _, err := read(); err == nil {
				break
			}
			select {
			case err := <-exited:
				process = nil
				data, _ := os.ReadFile(filepath.Join(home, "daemon.log"))
				t.Fatalf("actual unprivileged daemon failed: %v\n%s", err, data)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatal("actual daemon did not recover trusted HTTPS after startup/restart")
			}
			time.Sleep(100 * time.Millisecond)
		}
		before, err := read()
		if err != nil {
			t.Fatal(err)
		}
		request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+config.DaemonAddr+"/api/tls/renew", nil)
		response, err := (&http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}).Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("actual daemon leaf renewal returned HTTP %d", response.StatusCode)
		}
		after, err := read()
		if err != nil || after == before {
			t.Fatalf("actual daemon renewal did not replace its trusted leaf: %v", err)
		}
		stopProcess()
	}
}
