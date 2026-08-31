package docker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

type APIServer struct {
	socketPath string
	handler    http.Handler

	mu       sync.Mutex
	server   *http.Server
	listener net.Listener
	lease    endpointLease
	done     chan struct{}
	cancel   context.CancelFunc
	closed   chan struct{}
	closeErr error
}

func NewAPIServer(socketPath string, handler http.Handler) *APIServer {
	return &APIServer{socketPath: socketPath, handler: handler}
}

func (s *APIServer) SocketPath() string {
	return s.socketPath
}

func (s *APIServer) Start(ctx context.Context) error {
	if s.socketPath == "" {
		return errors.New("Porto Docker API endpoint path is empty")
	}
	listener, lease, err := listenDockerEndpoint(s.socketPath)
	if err != nil {
		return err
	}
	serverContext, cancel := context.WithCancel(ctx)
	server := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 30 * time.Second,
		BaseContext: func(net.Listener) context.Context {
			return context.WithValue(serverContext, dockerServerContextKey{}, serverContext)
		},
	}
	s.mu.Lock()
	s.listener = listener
	s.lease = lease
	s.server = server
	s.done = make(chan struct{})
	s.cancel = cancel
	s.closed = make(chan struct{})
	s.closeErr = nil
	done := s.done
	s.mu.Unlock()

	go func() {
		select {
		case <-ctx.Done():
			shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := s.Close(shutdownContext); err != nil {
				log.Printf("stop Docker API server: %v", err)
			}
		case <-done:
		}
	}()
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("Docker API server: %v", err)
		}
	}()
	return nil
}

func (s *APIServer) Close(ctx context.Context) error {
	s.mu.Lock()
	server := s.server
	lease := s.lease
	done := s.done
	cancel := s.cancel
	closed := s.closed
	s.server = nil
	s.listener = nil
	s.lease = nil
	s.done = nil
	s.cancel = nil
	s.mu.Unlock()
	if server == nil {
		if closed == nil {
			return nil
		}
		select {
		case <-closed:
			s.mu.Lock()
			closeErr := s.closeErr
			s.mu.Unlock()
			return closeErr
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
	if cancel != nil {
		cancel()
	}
	if done != nil {
		close(done)
	}
	shutdownErr := server.Shutdown(ctx)
	closeErr := shutdownErr
	if lease != nil {
		closeErr = errors.Join(shutdownErr, lease.Release(s.socketPath))
	}
	s.mu.Lock()
	s.closeErr = closeErr
	close(closed)
	s.mu.Unlock()
	return closeErr
}

func endpointListenError(path string, err error) error {
	return fmt.Errorf("listen on Porto Docker API endpoint %s: %w", path, err)
}

type endpointLease interface {
	Release(string) error
}

type dockerServerContextKey struct{}

func dockerServerContext(ctx context.Context) context.Context {
	if serverContext, ok := ctx.Value(dockerServerContextKey{}).(context.Context); ok {
		return serverContext
	}
	return context.Background()
}
