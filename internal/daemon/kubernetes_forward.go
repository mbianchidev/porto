package daemon

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mbianchidev/porto/internal/ports"
	"github.com/mbianchidev/porto/internal/process"
)

const kubernetesForwardStopTimeout = 5 * time.Second

type kubeForward struct {
	cmd          *exec.Cmd
	id           string
	contextName  string
	namespace    string
	resourceType string
	resourceName string
	port         int
	remotePort   int
	startedAt    time.Time
	managed      bool
	done         chan struct{}
	once         sync.Once
}

type kubernetesForwardSpec struct {
	id           string
	contextName  string
	namespace    string
	resourceType string
	resourceName string
	localPort    int
	remotePort   int
	managed      bool
}

type kubernetesPortForward struct {
	ID           string    `json:"id"`
	Context      string    `json:"context"`
	Namespace    string    `json:"namespace"`
	ResourceType string    `json:"resourceType"`
	ResourceName string    `json:"resourceName"`
	Address      string    `json:"address"`
	LocalPort    int       `json:"localPort"`
	RemotePort   int       `json:"remotePort"`
	StartedAt    time.Time `json:"startedAt"`
	Status       string    `json:"status"`
}

type createKubernetesPortForwardRequest struct {
	Namespace    string `json:"namespace"`
	ResourceType string `json:"resourceType"`
	ResourceName string `json:"resourceName"`
	LocalPort    int    `json:"localPort"`
	RemotePort   int    `json:"remotePort"`
}

func (s *Server) startServiceForward(
	key string,
	contextName string,
	namespace string,
	service string,
	localPort int,
	servicePort int,
) (*kubeForward, error) {
	return s.startKubernetesForward(key, kubernetesForwardSpec{
		contextName:  contextName,
		namespace:    namespace,
		resourceType: "service",
		resourceName: service,
		localPort:    localPort,
		remotePort:   servicePort,
	})
}

func (s *Server) startKubernetesForward(key string, spec kubernetesForwardSpec) (*kubeForward, error) {
	runContext := s.runtimeContext
	if runContext == nil {
		runContext = context.Background()
	}
	args := s.kubernetes.CommandArgs(
		spec.contextName,
		"port-forward",
		"--address", "127.0.0.1",
		"--namespace", spec.namespace,
		spec.resourceType+"/"+spec.resourceName,
		fmt.Sprintf("%d:%d", spec.localPort, spec.remotePort),
	)
	cmd, stdout, stderr, err := process.Command(runContext, "", "kubectl", args...)
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("start Kubernetes port forward: %w", err)
	}
	forward := &kubeForward{
		cmd:          cmd,
		id:           spec.id,
		contextName:  spec.contextName,
		namespace:    spec.namespace,
		resourceType: spec.resourceType,
		resourceName: spec.resourceName,
		port:         spec.localPort,
		remotePort:   spec.remotePort,
		startedAt:    time.Now().UTC(),
		managed:      spec.managed,
		done:         make(chan struct{}),
	}
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
			log.Printf("Kubernetes port forward %s stopped: %v", key, waitErr)
		}
	}()
	for range 40 {
		if !ports.IsFree(spec.localPort) {
			return forward, nil
		}
		select {
		case <-forward.done:
			return nil, fmt.Errorf("Kubernetes service forward %s exited before listening", key)
		case <-time.After(50 * time.Millisecond):
		}
	}
	_ = process.Terminate(cmd)
	return nil, fmt.Errorf("Kubernetes port forward %s did not listen on port %d", key, spec.localPort)
}

func (s *Server) captureKubernetesForward(key, stream string, reader interface {
	Read([]byte) (int, error)
	Close() error
}) {
	defer reader.Close()
	if err := process.Stream(reader, func(line string) error {
		if stream == "stderr" {
			log.Printf("Kubernetes port forward %s: %s", key, line)
		}
		return nil
	}); err != nil {
		log.Printf("read Kubernetes port forward %s %s: %v", key, stream, err)
	}
}

func (s *Server) kubernetesPortForwards(w http.ResponseWriter, r *http.Request) {
	contextName := strings.TrimSpace(runtimeContext(r))
	if contextName == "" {
		http.Error(w, "Kubernetes context is required", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	forwards := make([]kubernetesPortForward, 0)
	for _, forward := range s.kubeForwards {
		if forward.managed && forward.contextName == contextName && !kubernetesForwardDone(forward) {
			forwards = append(forwards, forward.snapshot())
		}
	}
	s.mu.Unlock()
	sort.Slice(forwards, func(i, j int) bool {
		if forwards[i].LocalPort != forwards[j].LocalPort {
			return forwards[i].LocalPort < forwards[j].LocalPort
		}
		return forwards[i].ID < forwards[j].ID
	})
	writeJSON(w, forwards)
}

func (s *Server) createKubernetesPortForward(w http.ResponseWriter, r *http.Request) {
	var request createKubernetesPortForwardRequest
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	spec, err := normalizeKubernetesPortForward(runtimeContext(r), request)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.kubeForwardMu.Lock()
	defer s.kubeForwardMu.Unlock()
	if existing := s.findManagedKubernetesForward(spec); existing != nil {
		http.Error(
			w,
			fmt.Sprintf("Kubernetes port forward already runs on 127.0.0.1:%d", existing.port),
			http.StatusConflict,
		)
		return
	}
	spec.localPort, err = s.allocateKubernetesForwardPort(r.Context(), spec.localPort)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	spec.id = kubernetesPortForwardID(spec)
	spec.managed = true
	forward, err := s.startKubernetesForward(kubernetesManualForwardKey(spec.contextName, spec.id), spec)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, forward.snapshot())
}

func (s *Server) stopKubernetesPortForward(w http.ResponseWriter, r *http.Request) {
	contextName := strings.TrimSpace(runtimeContext(r))
	id := strings.TrimSpace(r.PathValue("id"))
	if contextName == "" || id == "" || strings.ContainsAny(id, "/\\\r\n\x00") {
		http.Error(w, "Kubernetes context and port forward ID are required", http.StatusBadRequest)
		return
	}
	key := kubernetesManualForwardKey(contextName, id)
	s.kubeForwardMu.Lock()
	defer s.kubeForwardMu.Unlock()
	s.mu.Lock()
	forward := s.kubeForwards[key]
	if forward == nil || !forward.managed || forward.contextName != contextName {
		s.mu.Unlock()
		http.Error(w, "Kubernetes port forward not found", http.StatusNotFound)
		return
	}
	delete(s.kubeForwards, key)
	s.mu.Unlock()
	if err := stopKubernetesForwardList([]*kubeForward{forward}); err != nil {
		writeRuntimeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func normalizeKubernetesPortForward(
	contextName string,
	request createKubernetesPortForwardRequest,
) (kubernetesForwardSpec, error) {
	contextName = strings.TrimSpace(contextName)
	namespace := strings.TrimSpace(request.Namespace)
	resourceType := strings.ToLower(strings.TrimSpace(request.ResourceType))
	resourceName := strings.TrimSpace(request.ResourceName)
	switch resourceType {
	case "service", "services":
		resourceType = "service"
	case "pod", "pods":
		resourceType = "pod"
	case "deployment", "deployments":
		resourceType = "deployment"
	default:
		return kubernetesForwardSpec{}, errors.New("Kubernetes port forward resource must be a service, pod, or deployment")
	}
	if contextName == "" {
		return kubernetesForwardSpec{}, errors.New("Kubernetes context is required")
	}
	if strings.ContainsAny(contextName, "\r\n\x00") {
		return kubernetesForwardSpec{}, errors.New("invalid Kubernetes context")
	}
	if err := validateKubernetesForwardIdentifier("namespace", namespace); err != nil {
		return kubernetesForwardSpec{}, err
	}
	if err := validateKubernetesForwardIdentifier("resource name", resourceName); err != nil {
		return kubernetesForwardSpec{}, err
	}
	if request.LocalPort < 0 || request.LocalPort > 65535 {
		return kubernetesForwardSpec{}, errors.New("local port must be 0 or between 1 and 65535")
	}
	if request.RemotePort <= 0 || request.RemotePort > 65535 {
		return kubernetesForwardSpec{}, errors.New("remote port must be between 1 and 65535")
	}
	return kubernetesForwardSpec{
		contextName:  contextName,
		namespace:    namespace,
		resourceType: resourceType,
		resourceName: resourceName,
		localPort:    request.LocalPort,
		remotePort:   request.RemotePort,
	}, nil
}

func validateKubernetesForwardIdentifier(label, value string) error {
	if value == "" {
		return fmt.Errorf("Kubernetes %s is required", label)
	}
	if strings.HasPrefix(value, "-") || strings.HasSuffix(value, "-") {
		return fmt.Errorf("invalid Kubernetes %s", label)
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '.' {
			continue
		}
		return fmt.Errorf("invalid Kubernetes %s", label)
	}
	return nil
}

func (s *Server) findManagedKubernetesForward(spec kubernetesForwardSpec) *kubeForward {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, forward := range s.kubeForwards {
		if forward.managed &&
			forward.contextName == spec.contextName &&
			forward.namespace == spec.namespace &&
			forward.resourceType == spec.resourceType &&
			forward.resourceName == spec.resourceName &&
			forward.remotePort == spec.remotePort &&
			!kubernetesForwardDone(forward) {
			return forward
		}
	}
	return nil
}

func (s *Server) allocateKubernetesForwardPort(ctx context.Context, preferred int) (int, error) {
	used := map[int]bool{}
	if s.store != nil {
		var err error
		used, err = s.store.UsedPorts(ctx)
		if err != nil {
			return 0, fmt.Errorf("read reserved ports: %w", err)
		}
	}
	s.mu.Lock()
	for _, forward := range s.kubeForwards {
		used[forward.port] = true
	}
	s.mu.Unlock()
	if preferred > 0 {
		if used[preferred] || !ports.IsFree(preferred) {
			return 0, fmt.Errorf("local port %d is unavailable", preferred)
		}
		return preferred, nil
	}
	port, err := ports.Pick(0, 45000, used)
	if err != nil {
		return 0, fmt.Errorf("allocate localhost port for Kubernetes forward: %w", err)
	}
	return port, nil
}

func kubernetesPortForwardID(spec kubernetesForwardSpec) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		spec.contextName,
		spec.namespace,
		spec.resourceType,
		spec.resourceName,
		fmt.Sprintf("%d", spec.localPort),
		fmt.Sprintf("%d", spec.remotePort),
	}, "\x00")))
	return fmt.Sprintf("%x", sum[:8])
}

func kubernetesManualForwardKey(contextName, id string) string {
	return contextName + "/manual/" + id
}

func (forward *kubeForward) snapshot() kubernetesPortForward {
	return kubernetesPortForward{
		ID:           forward.id,
		Context:      forward.contextName,
		Namespace:    forward.namespace,
		ResourceType: forward.resourceType,
		ResourceName: forward.resourceName,
		Address:      "127.0.0.1",
		LocalPort:    forward.port,
		RemotePort:   forward.remotePort,
		StartedAt:    forward.startedAt,
		Status:       "running",
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
