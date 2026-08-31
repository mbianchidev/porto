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
	server := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 30 * time.Second,
	}
	s.mu.Lock()
	s.listener = listener
	s.lease = lease
	s.server = server
	s.done = make(chan struct{})
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
	s.server = nil
	s.listener = nil
	s.lease = nil
	s.done = nil
	s.mu.Unlock()
	if done != nil {
		close(done)
	}
	if server == nil {
		return nil
	}
	shutdownErr := server.Shutdown(ctx)
	if lease == nil {
		return shutdownErr
	}
	return errors.Join(shutdownErr, lease.Release(s.socketPath))
}

func endpointListenError(path string, err error) error {
	return fmt.Errorf("listen on Porto Docker API endpoint %s: %w", path, err)
}

type endpointLease interface {
	Release(string) error
}
