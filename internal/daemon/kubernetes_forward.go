package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/mbianchidev/porto/internal/ports"
	"github.com/mbianchidev/porto/internal/process"
)

const kubernetesForwardStopTimeout = 5 * time.Second

type kubeForward struct {
	cmd  *exec.Cmd
	port int
	done chan struct{}
	once sync.Once
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
		go s.captureKubernetesForward(key, "stdout", stdout)
		go s.captureKubernetesForward(key, "stderr", stderr)
		duplicate := &kubeForward{cmd: cmd, done: make(chan struct{})}
		go func() {
			_ = cmd.Wait()
			close(duplicate.done)
		}()
		_ = process.Terminate(cmd)
		if !waitForKubernetesForward(duplicate, kubernetesForwardStopTimeout) {
			_ = process.Kill(cmd)
			_ = waitForKubernetesForward(duplicate, kubernetesForwardStopTimeout)
		}
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
	return stopKubernetesForwardList(forwards)
}

func (s *Server) stopStaleKubernetesRawForwards(contextName string, desired map[string]bool) error {
	prefix := contextName + "/raw/"
	s.mu.Lock()
	forwards := make([]*kubeForward, 0)
	for key, forward := range s.kubeForwards {
		if strings.HasPrefix(key, prefix) && !desired[key] {
			forwards = append(forwards, forward)
			delete(s.kubeForwards, key)
		}
	}
	s.mu.Unlock()
	return stopKubernetesForwardList(forwards)
}

func stopKubernetesForwardList(forwards []*kubeForward) error {
	terminateErrors := make([]error, len(forwards))
	for index, forward := range forwards {
		if kubernetesForwardDone(forward) {
			continue
		}
		terminateErrors[index] = process.Terminate(forward.cmd)
	}
	var stopErrors []error
	for index, forward := range forwards {
		if forward.done == nil || waitForKubernetesForward(forward, kubernetesForwardStopTimeout) {
			continue
		}
		killErr := process.Kill(forward.cmd)
		if waitForKubernetesForward(forward, kubernetesForwardStopTimeout) {
			continue
		}
		stopErrors = append(stopErrors, errors.Join(
			terminateErrors[index],
			killErr,
			errors.New("timed out waiting for Kubernetes forward to exit"),
		))
	}
	return errors.Join(stopErrors...)
}

func kubernetesForwardDone(forward *kubeForward) bool {
	if forward == nil || forward.done == nil {
		return forward == nil
	}
	select {
	case <-forward.done:
		return true
	default:
		return false
	}
}

func waitForKubernetesForward(forward *kubeForward, timeout time.Duration) bool {
	if forward == nil || forward.done == nil {
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-forward.done:
		return true
	case <-timer.C:
		return false
	}
}
