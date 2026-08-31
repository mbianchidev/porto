//go:build windows

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
	"strings"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
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
		return errors.New("Porto Docker proxy pipe path is empty")
	}
	listener, err := winio.ListenPipe(p.socketPath, &winio.PipeConfig{
		SecurityDescriptor: "D:P(A;;GA;;;OW)",
		MessageMode:        false,
		InputBufferSize:    64 * 1024,
		OutputBufferSize:   64 * 1024,
	})
	if err != nil {
		return fmt.Errorf("listen on Porto Docker pipe %s: %w", p.socketPath, err)
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
			log.Printf("stop Docker API pipe proxy: %v", err)
		}
	}()
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("Docker API pipe proxy: %v", err)
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
	return server.Shutdown(ctx)
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
	if strings.HasPrefix(endpoint, "npipe://") || strings.HasPrefix(endpoint, `\\.\pipe\`) {
		pipePath := windowsPipePath(endpoint)
		if strings.EqualFold(pipePath, p.socketPath) {
			return nil, errors.New("Porto Docker proxy cannot use itself as the upstream endpoint")
		}
		target := &url.URL{Scheme: "http", Host: "docker"}
		proxy := httputil.NewSingleHostReverseProxy(target)
		proxy.FlushInterval = -1
		proxy.Transport = &http.Transport{
			DisableCompression: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return winio.DialPipeContext(ctx, pipePath)
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

func windowsPipePath(endpoint string) string {
	path := strings.TrimPrefix(endpoint, "npipe://")
	path = strings.ReplaceAll(path, "/", `\`)
	if strings.HasPrefix(path, `\\.\pipe\`) {
		return path
	}
	return `\\.\pipe\` + strings.TrimLeft(path, `\.`)
}
