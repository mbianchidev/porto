package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Proxy struct {
	socketPath string
	upstream   string

	mu       sync.Mutex
	server   *http.Server
	listener net.Listener
}

func NewProxy(socketPath, upstream string) *Proxy {
	return &Proxy{socketPath: socketPath, upstream: upstream}
}

func (p *Proxy) SocketPath() string {
	return p.socketPath
}

func (p *Proxy) Start(ctx context.Context) error {
	if p.socketPath == "" {
		return errors.New("Porto Docker proxy socket path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(p.socketPath), 0o700); err != nil {
		return fmt.Errorf("create Docker proxy directory: %w", err)
	}
	if err := removeStaleSocket(p.socketPath); err != nil {
		return err
	}
	listener, err := net.Listen("unix", p.socketPath)
	if err != nil {
		return fmt.Errorf("listen on Porto Docker socket %s: %w", p.socketPath, err)
	}
	if err := os.Chmod(p.socketPath, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(p.socketPath)
		return fmt.Errorf("protect Porto Docker socket: %w", err)
	}
	server := &http.Server{
		Handler:           http.HandlerFunc(p.serveHTTP),
		ReadHeaderTimeout: 30 * time.Second,
	}
	p.mu.Lock()
	p.listener = listener
	p.server = server
	p.mu.Unlock()

	go func() {
		<-ctx.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := p.Close(shutdownContext); err != nil {
			log.Printf("stop Docker API proxy: %v", err)
		}
	}()
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("Docker API proxy: %v", err)
		}
	}()
	return nil
}

func (p *Proxy) Close(ctx context.Context) error {
	p.mu.Lock()
	server := p.server
	p.server = nil
	p.listener = nil
	p.mu.Unlock()
	if server == nil {
		return nil
	}
	shutdownErr := server.Shutdown(ctx)
	removeErr := os.Remove(p.socketPath)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	return errors.Join(shutdownErr, removeErr)
}

func (p *Proxy) serveHTTP(w http.ResponseWriter, r *http.Request) {
	reverseProxy, err := p.reverseProxy()
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": err.Error()})
		return
	}
	reverseProxy.ServeHTTP(w, r)
}

func (p *Proxy) reverseProxy() (*httputil.ReverseProxy, error) {
	endpoint := strings.TrimSpace(p.upstream)
	if endpoint == "" {
		return nil, errors.New("no upstream Docker endpoint is configured; set PORTO_DOCKER_UPSTREAM or select a working Docker context")
	}
	if strings.HasPrefix(endpoint, "unix://") || filepath.IsAbs(endpoint) {
		socketPath := strings.TrimPrefix(endpoint, "unix://")
		if samePath(socketPath, p.socketPath) {
			return nil, errors.New("Porto Docker proxy cannot use itself as the upstream endpoint")
		}
		target := &url.URL{Scheme: "http", Host: "docker"}
		proxy := httputil.NewSingleHostReverseProxy(target)
		proxy.FlushInterval = -1
		proxy.Transport = &http.Transport{
			DisableCompression: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", socketPath)
			},
		}
		proxy.ErrorHandler = proxyError
		return proxy, nil
	}
	if strings.HasPrefix(endpoint, "tcp://") {
		endpoint = "http://" + strings.TrimPrefix(endpoint, "tcp://")
	}
	target, err := url.Parse(endpoint)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") {
		return nil, fmt.Errorf("unsupported upstream Docker endpoint %q", p.upstream)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	proxy.ErrorHandler = proxyError
	return proxy, nil
}

func proxyError(w http.ResponseWriter, _ *http.Request, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": fmt.Sprintf("Docker upstream request failed: %v", err)})
}

func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect Docker proxy socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to replace non-socket path %s", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale Docker proxy socket: %w", err)
	}
	return nil
}

func samePath(left, right string) bool {
	leftPath, leftErr := filepath.Abs(left)
	rightPath, rightErr := filepath.Abs(right)
	return leftErr == nil && rightErr == nil && filepath.Clean(leftPath) == filepath.Clean(rightPath)
}
