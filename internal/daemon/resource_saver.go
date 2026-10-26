package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	portodocker "github.com/mbianchidev/porto/internal/docker"
	"github.com/mbianchidev/porto/internal/resourcesaver"
)

type saverBackend struct {
	server   *Server
	mu       sync.Mutex
	previous map[string]saverIdleSample
	latest   resourcesaver.Snapshot
}

type saverIdleSample struct {
	Read     time.Time
	CPU      uint64
	Activity portodocker.ResourceSaverActivity
}

func (s *Server) activeResourceSaver() *resourcesaver.Manager {
	s.resourceSaverMu.Lock()
	defer s.resourceSaverMu.Unlock()
	return s.resourceSaver
}

func (s *Server) initializeResourceSaver(ctx context.Context) error {
	s.resourceSaverMu.Lock()
	defer s.resourceSaverMu.Unlock()
	if s.resourceSaver != nil {
		return nil
	}
	if s.store == nil || s.store.Directory() == "" {
		return errors.New("Persistent project storage is required for Resource Saver")
	}
	backend := &saverBackend{server: s, previous: map[string]saverIdleSample{}}
	manager, err := resourcesaver.New(ctx, filepath.Join(s.store.Directory(), "resource-saver.json"), backend)
	if err != nil {
		return err
	}
	s.resourceSaver, s.saverBackend = manager, backend
	s.docker.SetResourceSaver(manager)
	return nil
}

func (backend *saverBackend) Snapshot(ctx context.Context) (resourcesaver.Snapshot, error) {
	server := backend.server
	settings, err := server.store.Settings(ctx)
	if err != nil {
		return resourcesaver.Snapshot{}, err
	}
	if !settings.DockerEnabled || server.hasRuntimeOperations() {
		return resourcesaver.Snapshot{Message: "Docker is disabled or runtime/data operations are active"}, nil
	}
	inventory := server.docker.ContainerSnapshot()
	if !inventory.Available || inventory.Stale || time.Since(inventory.LastReconciledAt) > time.Minute {
		state := server.resourceSaver.State()
		if state.EngineToken != "" {
			owned, err := server.docker.ResourceSaverEngineOwned(state.EngineToken)
			if err != nil {
				return resourcesaver.Snapshot{}, err
			}
			if owned {
				return resourcesaver.Snapshot{Complete: true}, nil
			}
		}
		return resourcesaver.Snapshot{Message: inventory.Message}, nil
	}
	routes, _ := containerHTTPRoutes(inventory)
	applications := map[string]*resourcesaver.Application{}
	for _, container := range inventory.Containers {
		if !container.TaskPresent || container.State == "exited" || container.State == "created" {
			continue
		}
		id, name := saverApplicationID(container), strings.TrimPrefix(container.Name, "/")
		if container.ComposeProject != "" {
			name = container.ComposeProject
		}
		application := applications[id]
		if application == nil {
			application = &resourcesaver.Application{ID: id, Name: name, Eligible: true}
			applications[id] = application
		}
		application.Members = append(application.Members, resourcesaver.Member{
			ID: container.ID, Name: strings.TrimPrefix(container.Name, "/"), Identity: portodocker.ResourceSaverIdentity(container), PID: container.PID,
		})
		mode := container.Labels[portodocker.ResourceSaverLabel]
		switch mode {
		case "pause":
			if application.Mode != "" && application.Mode != resourcesaver.Pause {
				application.Eligible, application.Reason = false, "Compose members have conflicting suspension policies"
			}
			application.Mode = resourcesaver.Pause
		case "stop":
			if application.Mode != "" && application.Mode != resourcesaver.ScaleZero {
				application.Eligible, application.Reason = false, "Compose members have conflicting suspension policies"
			}
			application.Mode = resourcesaver.ScaleZero
		default:
			application.Eligible, application.Reason = false, "Every application member must explicitly opt in"
		}
		if container.Labels[portodocker.ManagedWorkloadLabel] != "true" || container.InventoryError != "" ||
			container.Labels["io.x-k8s.kind.cluster"] != "" || container.Labels["io.kubernetes.pod.name"] != "" {
			application.Eligible, application.Reason = false, "Unmanaged, Kubernetes, or incomplete workloads are excluded"
		}
		if saverHasStatefulMounts(container.MountDetails) && container.Labels[portodocker.ResourceSaverStatefulLabel] != "true" {
			application.Eligible, application.Reason = false, "Mounted/stateful workloads need separate explicit consent"
		}
		if timeout := container.Labels["dev.porto.resource-saver.idle-seconds"]; timeout != "" {
			seconds, err := strconv.Atoi(timeout)
			if err != nil || seconds < 1 || seconds > 86400 {
				application.Eligible, application.Reason = false, "Invalid per-container idle timeout"
			} else if application.IdleSeconds == 0 || seconds > application.IdleSeconds {
				application.IdleSeconds = seconds
			}
		}
		httpPort, err := strconv.Atoi(container.Labels["dev.porto.http-port"])
		if (err != nil || httpPort < 1) && container.ComposeProject == "" {
			application.Eligible, application.Reason = false, "Only explicitly routed HTTP applications support traffic-triggered wake"
		}
		if container.ComposeProject != "" && httpPort < 1 && container.Healthcheck == nil {
			application.Eligible, application.Reason = false, "Non-HTTP Compose dependencies require a health check"
		}
		for _, mapping := range container.NetworkDetails {
			if mapping.HostPort > 0 && (int(mapping.ContainerPort) != httpPort || mapping.Protocol != "" && mapping.Protocol != "tcp") {
				application.Eligible, application.Reason = false, "Additional published or raw TCP/UDP ports do not support safe first-connection wake"
			}
		}
		if container.State != "running" {
			application.Busy, application.Reason = true, "Paused or transitioning workloads are not adopted"
		} else if application.Eligible {
			busy, err := backend.busy(ctx, container.ID)
			if err != nil {
				application.Eligible, application.Reason = false, err.Error()
			} else {
				application.Busy = application.Busy || busy
			}
		}
	}
	for _, route := range routes {
		for _, application := range applications {
			for _, member := range application.Members {
				if member.ID == route.ContainerID {
					application.Routes = append(application.Routes, resourcesaver.Route{
						Hostname: route.Hostname, ContainerID: route.ContainerID, Host: route.Host, Port: route.Port,
					})
				}
			}
			owned := server.resourceSaver.State().Entries
			for _, container := range inventory.Containers {
				if container.ComposeProject == "" {
					continue
				}
				application := applications[saverApplicationID(container)]
				if application == nil || container.State == "running" {
					continue
				}
				ownedMember := false
				for _, entry := range owned {
					for _, member := range entry.Application.Members {
						ownedMember = ownedMember || member.ID == container.ID
					}
				}
				if !ownedMember {
					application.Eligible, application.Reason = false, "A stopped or manually paused Compose dependency is not owned by Resource Saver"
				}
			}
		}
	}
	snapshot := resourcesaver.Snapshot{Complete: true, EngineRunning: true}
	for _, application := range applications {
		if len(application.Routes) == 0 {
			application.Eligible = false
			if application.Reason == "" {
				application.Reason = "No unambiguous Porto HTTP route is available"
			}
		}
		snapshot.Applications = append(snapshot.Applications, *application)
	}
	backend.mu.Lock()
	backend.latest = snapshot
	backend.mu.Unlock()
	return snapshot, nil
}

func (backend *saverBackend) busy(ctx context.Context, id string) (bool, error) {
	sample, err := backend.server.docker.ContainerMetric(ctx, id)
	if err != nil {
		return false, err
	}
	if sample.CPU == nil {
		return false, errors.New("Backend CPU counters are required for idle detection")
	}
	activity, err := backend.server.docker.ResourceSaverActivity(ctx, id)
	if err != nil {
		return false, err
	}
	current := saverIdleSample{Read: sample.Read, CPU: sample.CPU.TotalUsage, Activity: activity}
	backend.mu.Lock()
	previous, exists := backend.previous[id]
	backend.previous[id] = current
	backend.mu.Unlock()
	if !exists || !sample.Read.After(previous.Read) || current.CPU < previous.CPU || current.Activity.PID != previous.Activity.PID {
		return true, nil
	}
	cpuMillicores := float64(current.CPU-previous.CPU) / float64(sample.Read.Sub(previous.Read)) * 1000
	if cpuMillicores > 5 {
		return true, nil
	}
	if current.Activity != previous.Activity {
		return true, nil
	}
	return false, nil
}

func saverApplicationID(container portodocker.Container) string {
	if container.ComposeProject != "" {
		return "compose:" + container.ComposeProject
	}
	return "container:" + container.ID
}

func saverHasStatefulMounts(mounts []portodocker.ContainerMount) bool {
	for _, mount := range mounts {
		if mount.Type == "volume" || mount.Type == "bind" &&
			mount.Destination != "/etc/hosts" && mount.Destination != "/etc/hostname" && mount.Destination != "/etc/resolv.conf" {
			return true
		}
	}
	return false
}

func (backend *saverBackend) Sleep(ctx context.Context, entry resourcesaver.Entry) error {
	return backend.server.docker.ResourceSaverSleep(ctx, entry)
}

func (backend *saverBackend) Resume(ctx context.Context, entry resourcesaver.Entry) error {
	settings, err := backend.server.store.Settings(ctx)
	if err != nil {
		return err
	}
	if !settings.DockerEnabled {
		return errors.New("Docker was disabled; Resource Saver will not start its workloads")
	}
	return backend.server.docker.ResourceSaverResume(ctx, entry)
}

func (backend *saverBackend) Ready(ctx context.Context, application resourcesaver.Application) (bool, error) {
	snapshot := backend.server.docker.ContainerSnapshot()
	if !snapshot.Available || snapshot.Stale {
		return false, nil
	}
	for _, member := range application.Members {
		found := false
		for _, container := range snapshot.Containers {
			if container.ID != member.ID {
				continue
			}
			found = true
			if portodocker.ResourceSaverIdentity(container) != member.Identity {
				return false, errors.New("Application identity changed while waiting for readiness")
			}
			if container.Health.Status == "unhealthy" {
				return false, fmt.Errorf("container %s failed readiness: %s", container.Name, container.Health.Output)
			}
			if container.State != "running" || container.Health.Status == "starting" {
				return false, nil
			}
		}
		if !found {
			return false, errors.New("Application container disappeared during wake")
		}
	}
	client := &http.Client{Timeout: 500 * time.Millisecond, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, route := range application.Routes {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(route.Host, strconv.Itoa(route.Port))+"/", nil)
		if err != nil {
			return false, err
		}
		response, err := client.Do(request)
		if err != nil {
			return false, nil
		}
		closeErr := response.Body.Close()
		if closeErr != nil {
			return false, closeErr
		}
		if response.StatusCode < 200 || response.StatusCode >= 400 {
			return false, nil
		}
	}
	return true, nil
}

func (backend *saverBackend) SuspendEngine(ctx context.Context, token string) error {
	server := backend.server
	if server.syncShares != nil {
		active, err := server.syncShares.Active()
		if err != nil {
			return err
		}
		if active {
			return errors.New("Active synchronized shares require the engine; pause their synchronization before engine suspension")
		}
	}
	if server.hasRuntimeOperations() || !server.beginRuntimeOperation() {
		return errors.New("Active runtime or data work prevents engine suspension")
	}
	defer server.endRuntimeOperation()
	settings, err := server.store.Settings(ctx)
	if err != nil {
		return err
	}
	if settings.KubernetesEnabled {
		clusters, err := server.clusters.List(ctx)
		if err != nil {
			return fmt.Errorf("Kubernetes inventory is incomplete: %w", err)
		}
		for _, cluster := range clusters {
			if cluster.State != "stopped" {
				return errors.New("Active or incompletely observed Kubernetes nodes prevent engine suspension")
			}
		}
	}
	return server.docker.SuspendResourceSaverEngine(ctx, token)
}

func (backend *saverBackend) WakeEngine(ctx context.Context, token string) error {
	settings, err := backend.server.store.Settings(ctx)
	if err != nil {
		return err
	}
	if !settings.DockerEnabled {
		return errors.New("Docker was disabled; its engine will not be woken")
	}
	return backend.server.docker.WakeResourceSaverEngine(ctx, token)
}

func (s *Server) resourceSaverStatus(w http.ResponseWriter, r *http.Request) {
	if err := s.initializeResourceSaver(context.Background()); err != nil {
		writeRuntimeError(w, err)
		return
	}
	s.saverBackend.mu.Lock()
	applications := s.saverBackend.latest.Applications
	s.saverBackend.mu.Unlock()
	writeJSON(w, map[string]any{"state": s.resourceSaver.State(), "applications": applications, "protocols": []string{"http", "https"}, "rawTCP": false})
}

func (s *Server) configureResourceSaver(w http.ResponseWriter, r *http.Request) {
	var configuration resourcesaver.Config
	if !decodeRuntimeJSON(w, r, &configuration) {
		return
	}
	if err := s.initializeResourceSaver(context.Background()); err != nil {
		writeRuntimeError(w, err)
		return
	}
	if err := s.resourceSaver.Configure(configuration); err != nil {
		writeRuntimeError(w, err)
		return
	}
	s.resourceSaverStatus(w, r)
}

func (s *Server) configureContainerResourceSaver(w http.ResponseWriter, r *http.Request) {
	if s.windowsWorker || windowsContainerMode(r) {
		writeRuntimeError(w, fmt.Errorf("%w: Resource Saver is available only for the Linux endpoint", portodocker.ErrUnsupported))
		return
	}
	var request struct {
		Mode     string `json:"mode"`
		Stateful bool   `json:"stateful"`
	}
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	if err := s.docker.SetResourceSaverPolicy(r.Context(), r.PathValue("id"), request.Mode, request.Stateful); err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "configured"})
}
