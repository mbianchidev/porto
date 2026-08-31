package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mbianchidev/porto/internal/kubernetes"
	"github.com/mbianchidev/porto/internal/ports"
	"github.com/mbianchidev/porto/internal/process"
)

type kubeForward struct {
	cmd  *exec.Cmd
	port int
	done chan struct{}
	once sync.Once
}

func (s *Server) attachServiceForwards(contextName string, services []kubernetes.Service) error {
	if contextName == "" {
		return nil
	}
	used, err := s.store.UsedPorts(context.Background())
	if err != nil {
		return err
	}
	s.mu.Lock()
	for _, forward := range s.kubeForwards {
		used[forward.port] = true
	}
	s.mu.Unlock()
	for serviceIndex := range services {
		service := &services[serviceIndex]
		if service.Type != "LoadBalancer" && service.Type != "NodePort" {
			continue
		}
		for portIndex := range service.Ports {
			servicePort := &service.Ports[portIndex]
			if servicePort.Protocol != "" && servicePort.Protocol != "TCP" {
				continue
			}
			key := contextName + "/" + service.Namespace + "/" + service.Name + "/" + strconv.Itoa(int(servicePort.Port))
			s.mu.Lock()
			existing := s.kubeForwards[key]
			s.mu.Unlock()
			if existing != nil {
				servicePort.LocalPort = existing.port
				continue
			}
			preferred := int(servicePort.NodePort)
			localPort, err := ports.Pick(preferred, 45000, used)
			if err != nil {
				return fmt.Errorf("allocate localhost port for %s/%s: %w", service.Namespace, service.Name, err)
			}
			used[localPort] = true
			forward, err := s.startServiceForward(
				key,
				contextName,
				service.Namespace,
				service.Name,
				localPort,
				int(servicePort.Port),
			)
			if err != nil {
				return err
			}
			servicePort.LocalPort = forward.port
		}
	}
	return nil
}

func (s *Server) startServiceForward(
	key string,
	contextName string,
	namespace string,
	service string,
	localPort int,
	servicePort int,
) (*kubeForward, error) {
	runContext := s.runtimeContext
	if runContext == nil {
		runContext = context.Background()
	}
	args := s.kubernetes.CommandArgs(
		contextName,
		"port-forward",
		"--address", "127.0.0.1",
		"--namespace", namespace,
		"service/"+service,
		fmt.Sprintf("%d:%d", localPort, servicePort),
	)
	cmd, stdout, stderr, err := process.Command(runContext, "", "kubectl", args...)
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("start Kubernetes service forward: %w", err)
	}
	forward := &kubeForward{cmd: cmd, port: localPort, done: make(chan struct{})}
	s.mu.Lock()
	if existing := s.kubeForwards[key]; existing != nil {
		s.mu.Unlock()
		_ = process.Terminate(cmd)
		return existing, nil
	}
	s.kubeForwards[key] = forward
	s.mu.Unlock()
	go s.captureKubernetesForward(key, "stdout", stdout)
	go s.captureKubernetesForward(key, "stderr", stderr)
	go func() {
		waitErr := cmd.Wait()
		s.mu.Lock()
		if s.kubeForwards[key] == forward {
			delete(s.kubeForwards, key)
		}
		s.mu.Unlock()
		forward.once.Do(func() { close(forward.done) })
		if waitErr != nil && runContext.Err() == nil {
			log.Printf("Kubernetes service forward %s stopped: %v", key, waitErr)
		}
	}()
	for range 40 {
		if !ports.IsFree(localPort) {
			return forward, nil
		}
		select {
		case <-forward.done:
			return nil, fmt.Errorf("Kubernetes service forward %s exited before listening", key)
		case <-time.After(50 * time.Millisecond):
		}
	}
	_ = process.Terminate(cmd)
	return nil, fmt.Errorf("Kubernetes service forward %s did not listen on port %d", key, localPort)
}

func (s *Server) captureKubernetesForward(key, stream string, reader interface {
	Read([]byte) (int, error)
	Close() error
}) {
	defer reader.Close()
	if err := process.Stream(reader, func(line string) error {
		if stream == "stderr" {
			log.Printf("Kubernetes service forward %s: %s", key, line)
		}
		return nil
	}); err != nil {
		log.Printf("read Kubernetes service forward %s %s: %v", key, stream, err)
	}
}

func (s *Server) stopKubernetesForwards(contextName string) error {
	prefix := contextName + "/"
	s.mu.Lock()
	forwards := make([]*kubeForward, 0)
	for key, forward := range s.kubeForwards {
		if strings.HasPrefix(key, prefix) {
			forwards = append(forwards, forward)
			delete(s.kubeForwards, key)
		}
	}
	s.mu.Unlock()
	var stopErrors []error
	for _, forward := range forwards {
		if err := process.Terminate(forward.cmd); err != nil {
			stopErrors = append(stopErrors, err)
		}
	}
	return errors.Join(stopErrors...)
}
