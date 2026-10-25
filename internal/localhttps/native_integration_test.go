//go:build linux || windows || darwin

package localhttps

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/certificates"
	"github.com/mbianchidev/porto/internal/process"
)

func TestNativePortlessHTTPSTrustRenewalAndOwnedRemoval(t *testing.T) {
	if os.Getenv("PORTO_TEST_NATIVE_HTTPS") != "1" {
		t.Skip("set PORTO_TEST_NATIVE_HTTPS=1 only on a disposable supported CI host")
	}
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		t.Fatal("native HTTPS mutates system integration and is restricted to disposable CI hosts")
	}
	binary := os.Getenv("PORTO_TEST_HTTPS_BINARY")
	if binary == "" {
		t.Fatal("PORTO_TEST_HTTPS_BINARY must identify the current built Porto CLI")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	home := t.TempDir()
	t.Setenv("PORTO_HOME", home)
	if runtime.GOOS != "darwin" {
		t.Setenv("HOME", t.TempDir())
	}
	t.Setenv("PORTO_TLS_ADDR", TargetAddress)
	t.Setenv("PORTO_TLS_PUBLIC_PORT", "")
	certificatePath := filepath.Join(home, "certificates", "porto.local.pem")
	authorityPath := filepath.Join(home, "certificates", "porto-root-ca.pem")
	hostname := "nested.synthetic-https.porto.localhost"
	manager := certificates.New(certificatePath, filepath.Join(home, "certificates", "porto.local-key.pem"),
		authorityPath, filepath.Join(home, "certificates", "porto-root-ca-key.pem"))
	first, err := manager.Ensure(hostname)
	if err != nil {
		t.Fatal(err)
	}
	if status := Snapshot(certificatePath); status.Installed {
		t.Fatal("refusing to replace a pre-existing HTTPS integration on the test host")
	}
	run := func(configuration string, args ...string) ([]byte, error) {
		commandContext, stopCommand := context.WithTimeout(ctx, 30*time.Second)
		defer stopCommand()
		started := time.Now()
		t.Logf("native HTTPS command starting: %v", args)
		output, err := runNativeHTTPSCommand(t, commandContext, binary, configuration, args...)
		t.Logf("native HTTPS command completed after %s: %v (error=%v)", time.Since(started), args, err)
		if commandContext.Err() != nil {
			err = errors.Join(err, commandContext.Err())
		}
		return output, err
	}
	mustRun := func(configuration string, args ...string) []byte {
		t.Helper()
		output, err := run(configuration, args...)
		if err != nil {
			t.Fatalf("native HTTPS %v: %v: %s", args, err, output)
		}
		return output
	}
	t.Cleanup(func() {
		for _, args := range [][]string{{"https", "uninstall"}, {"cert", "untrust"}} {
			cleanupContext, stop := context.WithTimeout(context.Background(), time.Minute)
			output, err := runNativeHTTPSCommand(t, cleanupContext, binary, home, args...)
			stop()
			if err != nil {
				t.Errorf("clean owned native HTTPS %v: %v: %s", args, err, output)
			}
		}
	})
	for _, address := range []string{ListenAddress, ListenAddressIPv6} {
		occupied, err := net.Listen("tcp", address)
		if err != nil {
			t.Fatalf("reserve synthetic conflict at %s: %v", address, err)
		}
		output, installErr := run(home, "https", "install")
		closeErr := occupied.Close()
		if errors.Is(installErr, context.DeadlineExceeded) || errors.Is(installErr, context.Canceled) {
			t.Fatalf("port conflict at %s timed out instead of being diagnosed: %v: %s", address, installErr, output)
		}
		if installErr == nil {
			t.Fatalf("occupied %s did not reject installation: %s", address, output)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		if status := Snapshot(certificatePath); status.Installed || status.Trusted {
			t.Fatalf("partial installation was not rolled back after %s: %+v", address, status)
		}
	}
	other := t.TempDir()
	t.Cleanup(func() {
		cleanupContext, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		output, err := runNativeHTTPSCommand(t, cleanupContext, binary, other, "cert", "untrust")
		if err != nil {
			t.Errorf("remove unrelated synthetic CA fixture: %v: %s", err, output)
		}
	})
	var imported struct {
		Trusted bool `json:"trusted"`
	}
	if err := json.Unmarshal(mustRun(other, "cert", "trust"), &imported); err != nil || !imported.Trusted {
		t.Fatalf("native CA import did not establish trust: %+v, %v", imported, err)
	}
	listener, err := net.Listen("tcp", TargetAddress)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><body>porto-native-https-ready</body></html>")
	}), ReadHeaderTimeout: 5 * time.Second}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(tls.NewListener(listener, manager.TLSConfig())) }()
	t.Cleanup(func() {
		_ = server.Close()
		if err := <-serverDone; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("close synthetic TLS router: %v", err)
		}
	})
	mustRun(home, "https", "install")
	read := func(address string) error {
		transport := &http.Transport{
			Proxy: nil, DisableKeepAlives: true,
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: hostname},
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", address)
			},
		}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
		response, err := client.Get("https://" + hostname + "/")
		if err != nil {
			return err
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			return err
		}
		if !strings.Contains(string(data), "porto-native-https-ready") {
			return errors.New("native HTTPS response did not render the synthetic route")
		}
		return nil
	}
	for _, address := range []string{ListenAddress, ListenAddressIPv6} {
		if err := read(address); err != nil {
			t.Fatalf("native system trust on %s: %v", address, err)
		}
	}

	if status := Snapshot(certificatePath); !status.Installed || !status.Listening || !status.Trusted || !status.BrowserTrusted {
		t.Fatalf("native integration is not operational: %+v", status)
	}
	if err := assertNativeHelperLeastPrivilege(); err != nil {
		t.Fatal(err)
	}
	browser := os.Getenv("PORTO_TEST_HTTPS_BROWSER")
	if browser == "" {
		t.Fatal("native HTTPS acceptance requires a supported real browser")
	}
	arguments := []string{"--headless", "--no-first-run", "--no-default-browser-check", "--disable-gpu",
		"--user-data-dir=" + filepath.Join(t.TempDir(), "browser-profile"), "--dump-dom", "https://" + hostname + "/"}
	arguments = append(nativeBrowserArguments(), arguments...)
	command := process.NewCommand(ctx, "", browser, arguments...)
	output, err := command.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "porto-native-https-ready") {
		t.Fatalf("browser did not trust and render the dotted, portless HTTPS route: %v: %s", err, output)
	}
	second, err := manager.Renew(hostname)
	if err != nil || second.Fingerprint == first.Fingerprint {
		t.Fatalf("server certificate renewal did not replace the leaf: %v", err)
	}
	for _, address := range []string{ListenAddress, ListenAddressIPv6} {
		if err := read(address); err != nil {
			t.Fatalf("renewal lost native trust on %s: %v", address, err)
		}
	}
	if err := crashNativeHelper(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		err := read(ListenAddress)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("owned helper did not recover after a crash: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
	mustRun(home, "https", "uninstall")
	mustRun(home, "https", "install")
	if err := read(ListenAddressIPv6); err != nil {
		t.Fatalf("helper reinstall lost IPv6 or trust: %v", err)
	}
	mustRun(home, "https", "uninstall")
	mustRun(home, "cert", "untrust")
	if status := Snapshot(certificatePath); status.Installed || status.Trusted {
		t.Fatalf("owned integration remained after removal: %+v", status)
	}
	var otherStatus struct {
		Trusted bool `json:"trusted"`
	}
	if err := json.Unmarshal(mustRun(other, "cert", "status"), &otherStatus); err != nil || !otherStatus.Trusted {
		t.Fatalf("uninstall removed an unrelated certificate: %+v, %v", otherStatus, err)
	}
}

func runNativeHTTPSCommand(t *testing.T, ctx context.Context, binary, configuration string, args ...string) ([]byte, error) {
	t.Helper()
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("PORTO_TEST_NATIVE_HTTPS") != "1" {
		return nil, errors.New("native HTTPS command fixtures are restricted to disposable CI hosts")
	}
	if output, handled, err := nativeHeadlessHTTPSCommand(t, ctx, binary, configuration, args...); handled {
		return output, err
	}
	command := process.NewCommand(ctx, "", binary, args...)
	command.Env = process.WithEnvironment(os.Environ(), "PORTO_HOME="+configuration)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		return output.Bytes(), err
	}
	stopConsent := nativeCertificateConsent(t, ctx, command.Process.Pid, filepath.Join(configuration, "certificates", "porto-root-ca.pem"))
	err := command.Wait()
	return output.Bytes(), errors.Join(err, stopConsent())
}
