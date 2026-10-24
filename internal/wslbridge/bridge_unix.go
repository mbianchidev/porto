//go:build !windows

package wslbridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func socketClient(socket string) *http.Client {
	return &http.Client{Timeout: time.Second, Transport: &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
}

func control(ctx context.Context, configuration Config, action string) error {
	client := socketClient(configuration.Socket)
	defer client.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://porto/_porto/"+action, nil)
	if err != nil {
		return err
	}
	request.Header.Set(OwnerHeader, configuration.Owner)
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1024))
	if response.StatusCode != http.StatusOK || response.Header.Get(OwnerHeader) != configuration.Owner {
		return errors.New("existing WSL socket is not owned by this integration")
	}
	return nil
}

func lock(ctx context.Context, path string, wait bool) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return file, nil
		}
		if !wait || (!errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN)) {
			_ = file.Close()
			return nil, fmt.Errorf("WSL bridge is already owned by another process: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func prepareDirectory(socket string) error {
	directory := filepath.Dir(socket)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || stat.Uid != uint32(os.Getuid()) {
		return errors.New("WSL socket directory must be owned by the current distribution user and must not be a symlink")
	}
	return os.Chmod(directory, 0o700)
}

func removeStaleSocket(configuration Config) error {
	info, err := os.Lstat(configuration.Socket)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	owner, err := os.ReadFile(configuration.Socket + ".owner")
	if err != nil || string(owner) != configuration.Owner || info.Mode()&os.ModeSocket == 0 {
		return errors.New("refusing to replace an unrelated WSL socket or file")
	}
	return os.Remove(configuration.Socket)
}

func Serve(ctx context.Context, configuration Config, dial func(context.Context) (net.Conn, error)) (err error) {
	if err := prepareDirectory(configuration.Socket); err != nil {
		return err
	}
	lease, err := lock(ctx, configuration.Socket+".serve.lock", false)
	if err != nil {
		return err
	}
	defer lease.Close()
	if err := removeStaleSocket(configuration); err != nil {
		return err
	}
	listener, err := net.Listen("unix", configuration.Socket)
	if err != nil {
		return fmt.Errorf("create private WSL Docker socket: %w", err)
	}
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	defer listener.Close()
	if err := os.Chmod(configuration.Socket, 0o600); err != nil {
		return err
	}
	info, err := os.Lstat(configuration.Socket)
	if err != nil {
		return err
	}
	if err := os.WriteFile(configuration.Socket+".owner", []byte(configuration.Owner), 0o600); err != nil {
		return err
	}
	defer func() {
		if current, statErr := os.Lstat(configuration.Socket); statErr == nil && os.SameFile(info, current) {
			err = errors.Join(err, os.Remove(configuration.Socket), os.Remove(configuration.Socket+".owner"))
		}
	}()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) }}
	defer transport.CloseIdleConnections()
	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.Out.URL.Scheme, request.Out.URL.Host = "http", "porto"
			request.Out.Header.Set(DistributionHeader, configuration.Distribution)
			request.Out.Header.Set(OwnerHeader, configuration.Owner)
		},
		Transport: transport,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			slog.Error("WSL Docker transport failed", "error", err)
			http.Error(w, "Porto's Windows Docker pipe is unavailable; start Porto, check the selected distribution, and retry", http.StatusBadGateway)
		},
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_porto/status" || r.URL.Path == "/_porto/stop" {
			if r.Method != http.MethodPost || r.Header.Get(OwnerHeader) != configuration.Owner {
				http.Error(w, "WSL bridge ownership mismatch", http.StatusForbidden)
				return
			}
			w.Header().Set(OwnerHeader, configuration.Owner)
			w.WriteHeader(http.StatusOK)
			if r.URL.Path == "/_porto/stop" {
				go cancel()
			}
			return
		}
		proxy.ServeHTTP(w, r)
	})
	var connectionMu sync.Mutex
	connections := map[net.Conn]bool{}
	server := &http.Server{
		Handler: handler, ReadHeaderTimeout: 10 * time.Second,
		ConnState: func(connection net.Conn, state http.ConnState) {
			connectionMu.Lock()
			defer connectionMu.Unlock()
			if state == http.StateClosed {
				delete(connections, connection)
			} else {
				connections[connection] = true
			}
		},
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		_ = server.Close()
		connectionMu.Lock()
		defer connectionMu.Unlock()
		for connection := range connections {
			_ = connection.Close()
		}
	}()
	err = server.Serve(listener)
	cancel()
	<-stopped
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func Ensure(ctx context.Context, path string, configuration Config) error {
	if err := prepareDirectory(configuration.Socket); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	lease, err := lock(ctx, configuration.Socket+".startup.lock", true)
	if err != nil {
		return err
	}
	defer lease.Close()
	if err := control(ctx, configuration, "status"); err == nil {
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(filepath.Dir(configuration.Socket), "bridge.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	command := exec.Command(executable, "serve", path)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	command.Stdout, command.Stderr = log, log
	if err := command.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	for {
		if err := control(ctx, configuration, "status"); err == nil {
			return nil
		}
		select {
		case err := <-done:
			return fmt.Errorf("WSL bridge stopped before becoming ready; inspect %s: %w", log.Name(), err)
		case <-ctx.Done():
			return fmt.Errorf("WSL bridge did not become ready; inspect %s: %w", log.Name(), ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func Stop(ctx context.Context, configuration Config) error {
	if err := control(ctx, configuration, "stop"); err != nil {
		if _, statErr := os.Lstat(configuration.Socket); errors.Is(statErr, os.ErrNotExist) {
			return nil
		}
		lease, lockErr := lock(ctx, configuration.Socket+".serve.lock", false)
		if lockErr != nil {
			return errors.Join(err, lockErr)
		}
		defer lease.Close()
		if err := removeStaleSocket(configuration); err != nil {
			return err
		}
		return nil
	}
	for range 100 {
		if _, err := os.Lstat(configuration.Socket); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	return errors.New("WSL bridge did not release its owned socket")
}

func Endpoint(configuration Config) string {
	return "unix://" + strings.ReplaceAll(configuration.Socket, "\\", "/")
}
