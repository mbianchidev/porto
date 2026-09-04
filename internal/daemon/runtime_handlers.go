package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/app"
	"github.com/mbianchidev/porto/internal/config"
	portodocker "github.com/mbianchidev/porto/internal/docker"
	"github.com/mbianchidev/porto/internal/kubernetes"
	"github.com/mbianchidev/porto/internal/vm"
)

const maxRuntimeRequestBytes = 2 * 1024 * 1024

func (s *Server) runtimeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/runtime", s.runtimeStatus)
	mux.HandleFunc("GET /api/runtime/features", s.runtimeFeatures)
	mux.HandleFunc("POST /api/runtime/features/{feature}/{action}", s.setRuntimeFeature)
	mux.HandleFunc("GET /api/runtime/providers", s.runtimeProviders)
	mux.HandleFunc("POST /api/runtime/providers/{provider}/install", s.installRuntimeProvider)
	mux.HandleFunc("GET /api/activity/resources", s.activityResources)
	mux.HandleFunc("GET /api/docker/status", s.dockerStatus)
	mux.HandleFunc("POST /api/docker/engine/install", s.requireRuntime("docker", s.installDockerEngine))
	mux.HandleFunc("POST /api/docker/context/install", s.requireRuntime("docker", s.installDockerContext))
	mux.HandleFunc("GET /api/docker/containers", s.requireRuntime("docker", s.dockerContainers))
	mux.HandleFunc("POST /api/docker/containers", s.requireRuntime("docker", s.createDockerContainer))
	mux.HandleFunc("GET /api/docker/containers/snapshot", s.requireRuntime("docker", s.dockerContainerSnapshot))
	mux.HandleFunc("GET /api/docker/containers/events", s.requireRuntime("docker", s.dockerContainerEvents))
	mux.HandleFunc("GET /api/docker/containers/stats", s.requireRuntime("docker", s.dockerContainerStats))
	mux.HandleFunc("GET /api/docker/containers/{id}", s.requireRuntime("docker", s.dockerContainer))
	mux.HandleFunc("GET /api/docker/containers/{id}/logs", s.requireRuntime("docker", s.dockerContainerLogs))
	mux.HandleFunc("GET /api/docker/containers/{id}/terminal", s.requireRuntime("docker", s.dockerContainerTerminal))
	mux.HandleFunc("POST /api/docker/containers/{id}/exec", s.requireRuntime("docker", s.dockerContainerExec))
	mux.HandleFunc("POST /api/docker/containers/{id}/{action}", s.requireRuntime("docker", s.dockerContainerAction))
	mux.HandleFunc("GET /api/docker/images", s.requireRuntime("docker", s.dockerImages))
	mux.HandleFunc("GET /api/docker/images/{id}", s.requireRuntime("docker", s.dockerImage))
	mux.HandleFunc("POST /api/docker/images/pull", s.requireRuntime("docker", s.dockerPullImage))
	mux.HandleFunc("DELETE /api/docker/images/{id}", s.requireRuntime("docker", s.dockerRemoveImage))
	mux.HandleFunc("GET /api/docker/builds", s.requireRuntime("docker", s.dockerBuilds))
	mux.HandleFunc("POST /api/docker/builds", s.requireRuntime("docker", s.dockerBuild))
	mux.HandleFunc("GET /api/docker/networks", s.requireRuntime("docker", s.dockerNetworks))
	mux.HandleFunc("GET /api/docker/networks/{name}", s.requireRuntime("docker", s.dockerNetwork))
	mux.HandleFunc("POST /api/docker/networks", s.requireRuntime("docker", s.dockerCreateNetwork))
	mux.HandleFunc("DELETE /api/docker/networks/{name}", s.requireRuntime("docker", s.dockerRemoveNetwork))
	mux.HandleFunc("GET /api/docker/volumes", s.requireRuntime("docker", s.dockerVolumes))
	mux.HandleFunc("GET /api/docker/volumes/{name}", s.requireRuntime("docker", s.dockerVolume))
	mux.HandleFunc("POST /api/docker/volumes", s.requireRuntime("docker", s.dockerCreateVolume))
	mux.HandleFunc("DELETE /api/docker/volumes/{name}", s.requireRuntime("docker", s.dockerRemoveVolume))

	mux.HandleFunc("GET /api/kubernetes/status", s.kubernetesStatus)
	mux.HandleFunc("GET /api/kubernetes/contexts", s.requireRuntime("kubernetes", s.kubernetesContexts))
	mux.HandleFunc("GET /api/kubernetes/pods", s.requireRuntime("kubernetes", s.kubernetesPods))
	mux.HandleFunc("GET /api/kubernetes/services", s.requireRuntime("kubernetes", s.kubernetesServices))
	mux.HandleFunc("GET /api/kubernetes/configmaps", s.requireRuntime("kubernetes", s.kubernetesConfigMaps))
	mux.HandleFunc("GET /api/kubernetes/configmaps/{namespace}/{name}", s.requireRuntime("kubernetes", s.kubernetesConfigMap))
	mux.HandleFunc("GET /api/kubernetes/secrets", s.requireRuntime("kubernetes", s.kubernetesSecrets))
	mux.HandleFunc("GET /api/kubernetes/nodes", s.requireRuntime("kubernetes", s.kubernetesNodes))
	mux.HandleFunc("GET /api/kubernetes/persistent-volumes", s.requireRuntime("kubernetes", s.kubernetesPersistentVolumes))
	mux.HandleFunc("GET /api/kubernetes/persistent-volume-claims", s.requireRuntime("kubernetes", s.kubernetesPersistentVolumeClaims))
	mux.HandleFunc("GET /api/kubernetes/gateway-classes", s.requireRuntime("kubernetes", s.kubernetesGatewayClasses))
	mux.HandleFunc("GET /api/kubernetes/gateways", s.requireRuntime("kubernetes", s.kubernetesGateways))
	mux.HandleFunc("GET /api/kubernetes/http-routes", s.requireRuntime("kubernetes", s.kubernetesHTTPRoutes))
	mux.HandleFunc("GET /api/kubernetes/pods/{namespace}/{pod}", s.requireRuntime("kubernetes", s.kubernetesPod))
	mux.HandleFunc("GET /api/kubernetes/pods/{namespace}/{pod}/logs", s.requireRuntime("kubernetes", s.kubernetesPodLogs))
	mux.HandleFunc("POST /api/kubernetes/pods/{namespace}/{pod}/exec", s.requireRuntime("kubernetes", s.kubernetesPodExec))
	mux.HandleFunc("POST /api/kubernetes/pods/{namespace}/{pod}/debug", s.requireRuntime("kubernetes", s.kubernetesPodDebug))
	mux.HandleFunc("GET /api/kubernetes/pods/{namespace}/{pod}/debug/{container}", s.requireRuntime("kubernetes", s.kubernetesPodDebugStatus))
	mux.HandleFunc("GET /api/kubernetes/pods/{namespace}/{pod}/terminal", s.requireRuntime("kubernetes", s.kubernetesPodTerminal))
	mux.HandleFunc("GET /api/kubernetes/pods/{namespace}/{pod}/capabilities", s.requireRuntime("kubernetes", s.kubernetesPodCapabilities))
	mux.HandleFunc("GET /api/kubernetes/pods/{namespace}/{pod}/files", s.requireRuntime("kubernetes", s.kubernetesPodFiles))
	mux.HandleFunc("GET /api/kubernetes/pods/{namespace}/{pod}/file", s.requireRuntime("kubernetes", s.kubernetesPodFile))
	mux.HandleFunc("PUT /api/kubernetes/pods/{namespace}/{pod}/file", s.requireRuntime("kubernetes", s.kubernetesWritePodFile))
	mux.HandleFunc("DELETE /api/kubernetes/pods/{namespace}/{pod}/file", s.requireRuntime("kubernetes", s.kubernetesDeletePodFile))
	mux.HandleFunc("GET /api/kubernetes/pods/{namespace}/{pod}/stats", s.requireRuntime("kubernetes", s.kubernetesPodStats))
	mux.HandleFunc("GET /api/kubernetes/pods/{namespace}/{pod}/events", s.requireRuntime("kubernetes", s.kubernetesPodEvents))
	mux.HandleFunc("GET /api/kubernetes/pods/{namespace}/{pod}/manifest", s.requireRuntime("kubernetes", s.kubernetesPodManifest))
	mux.HandleFunc("GET /api/kubernetes/clusters", s.requireRuntime("kubernetes", s.kubernetesClusters))
	mux.HandleFunc("POST /api/kubernetes/clusters", s.requireRuntime("kubernetes", s.createKubernetesCluster))
	mux.HandleFunc("POST /api/kubernetes/clusters/{name}/start", s.requireRuntime("kubernetes", s.startKubernetesCluster))
	mux.HandleFunc("POST /api/kubernetes/clusters/{name}/stop", s.requireRuntime("kubernetes", s.stopKubernetesCluster))
	mux.HandleFunc("POST /api/kubernetes/clusters/{name}/rename", s.requireRuntime("kubernetes", s.renameKubernetesCluster))
	mux.HandleFunc("POST /api/kubernetes/clusters/{name}/node-groups/{group}", s.requireRuntime("kubernetes", s.scaleKubernetesNodeGroup))
	mux.HandleFunc("POST /api/kubernetes/clusters/{name}/images/import", s.requireRuntime("kubernetes", s.importKubernetesImage))
	mux.HandleFunc("GET /api/kubernetes/clusters/{name}/terminal", s.requireRuntime("kubernetes", s.kubernetesClusterTerminal))
	mux.HandleFunc("DELETE /api/kubernetes/clusters/{name}", s.requireRuntime("kubernetes", s.deleteKubernetesCluster))

	mux.HandleFunc("GET /api/vms/status", s.vmStatus)
	mux.HandleFunc("GET /api/vms/images", s.vmImages)
	mux.HandleFunc("GET /api/vms/instances", s.requireRuntime("vms", s.vmInstances))
	mux.HandleFunc("POST /api/vms/instances", s.requireRuntime("vms", s.createVM))
	mux.HandleFunc("POST /api/vms/instances/{name}/start", s.requireRuntime("vms", s.startVM))
	mux.HandleFunc("POST /api/vms/instances/{name}/stop", s.requireRuntime("vms", s.stopVM))
	mux.HandleFunc("POST /api/vms/instances/{name}/exec", s.requireRuntime("vms", s.execVM))
	mux.HandleFunc("GET /api/vms/instances/{name}/terminal", s.requireRuntime("vms", s.vmTerminal))
	mux.HandleFunc("POST /api/vms/instances/{name}/snapshot", s.requireRuntime("vms", s.snapshotVM))
	mux.HandleFunc("POST /api/vms/instances/{name}/restore", s.requireRuntime("vms", s.restoreVMSnapshot))
	mux.HandleFunc("DELETE /api/vms/instances/{name}", s.requireRuntime("vms", s.deleteVM))
}

func (s *Server) runtimeStatus(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.Settings(r.Context())
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	dockerStatus := portodocker.Status{Enabled: settings.DockerEnabled, Message: "Docker runtime is disabled"}
	if settings.DockerEnabled {
		dockerStatus = s.docker.Status(r.Context(), s.dockerSocket)
		dockerStatus.Enabled = true
		dockerStatus = s.dockerEndpointStatus(dockerStatus)
	}
	kubernetesStatus := kubernetes.Status{Enabled: settings.KubernetesEnabled, Message: "Kubernetes runtime is disabled"}
	if settings.KubernetesEnabled {
		kubernetesStatus = s.kubernetes.Status(r.Context(), r.URL.Query().Get("context"))
		kubernetesStatus.Enabled = true
	}
	vmStatus := vm.Status{Enabled: settings.VMsEnabled, Provider: "lima", Message: "VM runtime is disabled"}
	if settings.VMsEnabled {
		vmStatus = s.vms.Status(r.Context())
		vmStatus.Enabled = true
	}
	writeJSON(w, map[string]any{
		"docker":     dockerStatus,
		"kubernetes": kubernetesStatus,
		"vms":        vmStatus,
	})
}

func (s *Server) runtimeProviders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.providers.Status(r.Context()))
}

func (s *Server) installRuntimeProvider(w http.ResponseWriter, r *http.Request) {
	status, err := s.providers.Install(r.Context(), r.PathValue("provider"))
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, status)
}

func (s *Server) dockerStatus(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.Settings(r.Context())
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	if !settings.DockerEnabled {
		writeJSON(w, portodocker.Status{Message: "Docker runtime is disabled"})
		return
	}
	status := s.docker.Status(r.Context(), s.dockerSocket)
	status.Enabled = true
	status = s.dockerEndpointStatus(status)
	writeJSON(w, status)
}

func (s *Server) installDockerEngine(w http.ResponseWriter, r *http.Request) {
	status, err := s.docker.InstallEngine(r.Context())
	writeRuntimeResult(w, status, err)
}

func (s *Server) installDockerContext(w http.ResponseWriter, r *http.Request) {
	err := s.docker.InstallContext(r.Context(), s.dockerSocket)
	writeRuntimeResult(w, map[string]string{
		"context":  "porto",
		"endpoint": portodocker.EndpointURL(s.dockerSocket),
	}, err)
}

func (s *Server) dockerContainers(w http.ResponseWriter, r *http.Request) {
	value, err := s.docker.Containers(r.Context())
	writeRuntimeResult(w, value, err)
}

func (s *Server) createDockerContainer(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name          string `json:"name"`
		Image         string `json:"image"`
		HostPort      int    `json:"hostPort"`
		ContainerPort int    `json:"containerPort"`
		HealthCommand string `json:"healthCommand"`
	}
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	request.Image = strings.TrimSpace(request.Image)
	request.HealthCommand = strings.TrimSpace(request.HealthCommand)
	if request.Name == "" || request.Image == "" {
		http.Error(w, "container name and image are required", http.StatusBadRequest)
		return
	}
	if (request.HostPort == 0) != (request.ContainerPort == 0) {
		http.Error(w, "host port and container port must be provided together", http.StatusBadRequest)
		return
	}
	for name, port := range map[string]int{"host port": request.HostPort, "container port": request.ContainerPort} {
		if port < 0 || port > 65535 {
			http.Error(w, name+" must be between 1 and 65535", http.StatusBadRequest)
			return
		}
	}
	if strings.ContainsAny(request.HealthCommand, "\r\n\x00") {
		http.Error(w, "health command cannot contain line breaks", http.StatusBadRequest)
		return
	}
	createRequest := portodocker.CreateContainerRequest{
		Name:  request.Name,
		Image: request.Image,
	}
	if request.HostPort > 0 {
		createRequest.Publish = []string{
			fmt.Sprintf("127.0.0.1:%d:%d/tcp", request.HostPort, request.ContainerPort),
		}
	}
	if request.HealthCommand != "" {
		createRequest.Healthcheck = &portodocker.ContainerHealthcheck{
			Test:     []string{"CMD-SHELL", request.HealthCommand},
			Interval: 30 * time.Second,
			Timeout:  5 * time.Second,
			Retries:  3,
		}
	}
	id, err := s.docker.RunContainer(r.Context(), createRequest)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, map[string]string{
		"id":     id,
		"name":   request.Name,
		"status": "running",
	})
}

func (s *Server) dockerContainerSnapshot(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.docker.ContainerSnapshot())
}

func (s *Server) dockerContainerEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeRuntimeError(w, errors.New("container event streaming is unavailable"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	if _, err := io.WriteString(w, "retry: 1000\n\n"); err != nil {
		return
	}
	flusher.Flush()

	updates, unsubscribe := s.docker.SubscribeContainerSnapshots()
	defer unsubscribe()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case snapshot, ok := <-updates:
			if !ok {
				return
			}
			payload, err := json.Marshal(snapshot)
			if err != nil {
				return
			}
			if _, err := fmt.Fprintf(w, "id: %d\nevent: snapshot\ndata: %s\n\n", snapshot.Revision, payload); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Server) dockerContainer(w http.ResponseWriter, r *http.Request) {
	value, err := s.docker.InspectContainer(r.Context(), r.PathValue("id"))
	writeRuntimeResult(w, value, err)
}

func (s *Server) dockerContainerLogs(w http.ResponseWriter, r *http.Request) {
	tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	output, err := s.docker.ContainerLogs(r.Context(), r.PathValue("id"), tail)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(output)
}

func (s *Server) dockerContainerExec(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Command []string `json:"command"`
		Stdin   string   `json:"stdin"`
	}
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	output, err := s.docker.ExecContainer(r.Context(), r.PathValue("id"), request.Command, []byte(request.Stdin))
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"output": string(output)})
}

func (s *Server) dockerContainerStats(w http.ResponseWriter, r *http.Request) {
	value, err := s.docker.ContainerStats(r.Context())
	writeRuntimeResult(w, value, err)
}

func (s *Server) dockerContainerAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	if strings.HasPrefix(action, "remove") && s.clusters != nil {
		name, err := s.docker.ContainerName(r.Context(), r.PathValue("id"))
		if err != nil {
			writeRuntimeError(w, err)
			return
		}
		if err := s.clusters.ProtectContainerRemoval(r.Context(), name); err != nil {
			writeRuntimeError(w, err)
			return
		}
	}
	if err := s.docker.ContainerAction(r.Context(), r.PathValue("id"), action); err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) dockerImages(w http.ResponseWriter, r *http.Request) {
	value, err := s.docker.Images(r.Context())
	writeRuntimeResult(w, value, err)
}

func (s *Server) dockerImage(w http.ResponseWriter, r *http.Request) {
	value, err := s.docker.InspectImage(r.Context(), r.PathValue("id"))
	writeRuntimeResult(w, value, err)
}

func (s *Server) dockerPullImage(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Reference string `json:"reference"`
	}
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	if err := s.docker.PullImage(r.Context(), request.Reference, ""); err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, map[string]string{"status": "pulled"})
}

func (s *Server) dockerRemoveImage(w http.ResponseWriter, r *http.Request) {
	force := queryBool(r, "force")
	if err := s.docker.RemoveImage(r.Context(), r.PathValue("id"), force); err != nil {
		writeRuntimeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) dockerBuilds(w http.ResponseWriter, r *http.Request) {
	value, err := s.docker.Builds(r.Context())
	writeRuntimeResult(w, value, err)
}

func (s *Server) dockerBuild(w http.ResponseWriter, r *http.Request) {
	var request portodocker.BuildRequest
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	output, err := s.docker.Build(r.Context(), request)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, map[string]string{"status": "built", "output": string(output)})
}

func (s *Server) dockerNetworks(w http.ResponseWriter, r *http.Request) {
	value, err := s.docker.Networks(r.Context())
	writeRuntimeResult(w, value, err)
}

func (s *Server) dockerNetwork(w http.ResponseWriter, r *http.Request) {
	value, err := s.docker.InspectNetwork(r.Context(), r.PathValue("name"))
	writeRuntimeResult(w, value, err)
}

func (s *Server) dockerCreateNetwork(w http.ResponseWriter, r *http.Request) {
	var request portodocker.CreateNetworkRequest
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	if _, err := s.docker.CreateNetwork(r.Context(), request); err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, map[string]string{"status": "created"})
}

func (s *Server) dockerRemoveNetwork(w http.ResponseWriter, r *http.Request) {
	if err := s.docker.RemoveNetwork(r.Context(), r.PathValue("name")); err != nil {
		writeRuntimeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) dockerVolumes(w http.ResponseWriter, r *http.Request) {
	value, err := s.docker.Volumes(r.Context())
	writeRuntimeResult(w, value, err)
}

func (s *Server) dockerVolume(w http.ResponseWriter, r *http.Request) {
	value, err := s.docker.InspectVolume(r.Context(), r.PathValue("name"))
	writeRuntimeResult(w, value, err)
}

func (s *Server) dockerCreateVolume(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name   string `json:"name"`
		Driver string `json:"driver"`
	}
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	if _, err := s.docker.CreateVolume(r.Context(), request.Name, request.Driver, nil); err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, map[string]string{"status": "created"})
}

func (s *Server) dockerRemoveVolume(w http.ResponseWriter, r *http.Request) {
	if err := s.docker.RemoveVolume(r.Context(), r.PathValue("name"), queryBool(r, "force")); err != nil {
		writeRuntimeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) kubernetesStatus(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.Settings(r.Context())
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	if !settings.KubernetesEnabled {
		writeJSON(w, kubernetes.Status{Message: "Kubernetes runtime is disabled"})
		return
	}
	status := s.kubernetes.Status(r.Context(), r.URL.Query().Get("context"))
	status.Enabled = true
	writeJSON(w, status)
}

func (s *Server) kubernetesContexts(w http.ResponseWriter, r *http.Request) {
	value, err := s.kubernetes.Contexts(r.Context())
	writeRuntimeResult(w, value, err)
}

func (s *Server) kubernetesPods(w http.ResponseWriter, r *http.Request) {
	value, err := s.kubernetes.Pods(r.Context(), runtimeContext(r), r.URL.Query().Get("namespace"))
	writeRuntimeResult(w, value, err)
}

func (s *Server) kubernetesServices(w http.ResponseWriter, r *http.Request) {
	contextName := runtimeContext(r)
	namespace := r.URL.Query().Get("namespace")
	value, err := s.kubernetes.Services(r.Context(), contextName, namespace)
	if err == nil {
		managed, managedErr := s.managedKubernetesContext(r.Context(), contextName)
		if managedErr != nil {
			err = managedErr
		} else if managed {
			if namespace == "" || namespace == "all" {
				err = s.reconcileServiceRoutes(r.Context(), contextName, value)
			} else {
				allServices, allErr := s.kubernetes.Services(r.Context(), contextName, "")
				if allErr == nil {
					allErr = s.reconcileServiceRoutes(r.Context(), contextName, allServices)
				}
				err = errors.Join(allErr, s.decorateServiceRoutes(r.Context(), contextName, value))
			}
		}
	}
	writeRuntimeResult(w, value, err)
}

func (s *Server) kubernetesConfigMaps(w http.ResponseWriter, r *http.Request) {
	value, err := s.kubernetes.ConfigMaps(r.Context(), runtimeContext(r), r.URL.Query().Get("namespace"))
	writeRuntimeResult(w, value, err)
}

func (s *Server) kubernetesConfigMap(w http.ResponseWriter, r *http.Request) {
	value, err := s.kubernetes.ConfigMap(
		r.Context(),
		runtimeContext(r),
		r.PathValue("namespace"),
		r.PathValue("name"),
	)
	writeRuntimeResult(w, value, err)
}

func (s *Server) kubernetesSecrets(w http.ResponseWriter, r *http.Request) {
	value, err := s.kubernetes.Secrets(r.Context(), runtimeContext(r), r.URL.Query().Get("namespace"))
	writeRuntimeResult(w, value, err)
}

func (s *Server) kubernetesNodes(w http.ResponseWriter, r *http.Request) {
	value, err := s.kubernetes.Nodes(r.Context(), runtimeContext(r))
	writeRuntimeResult(w, value, err)
}

func (s *Server) kubernetesPersistentVolumes(w http.ResponseWriter, r *http.Request) {
	value, err := s.kubernetes.PersistentVolumes(r.Context(), runtimeContext(r))
	writeRuntimeResult(w, value, err)
}

func (s *Server) kubernetesPersistentVolumeClaims(w http.ResponseWriter, r *http.Request) {
	value, err := s.kubernetes.PersistentVolumeClaims(r.Context(), runtimeContext(r), r.URL.Query().Get("namespace"))
	writeRuntimeResult(w, value, err)
}

func (s *Server) kubernetesGatewayClasses(w http.ResponseWriter, r *http.Request) {
	value, err := s.kubernetes.GatewayClasses(r.Context(), runtimeContext(r))
	writeRuntimeResult(w, value, err)
}

func (s *Server) kubernetesGateways(w http.ResponseWriter, r *http.Request) {
	value, err := s.kubernetes.Gateways(r.Context(), runtimeContext(r), r.URL.Query().Get("namespace"))
	writeRuntimeResult(w, value, err)
}

func (s *Server) kubernetesHTTPRoutes(w http.ResponseWriter, r *http.Request) {
	value, err := s.kubernetes.HTTPRoutes(r.Context(), runtimeContext(r), r.URL.Query().Get("namespace"))
	writeRuntimeResult(w, value, err)
}

func (s *Server) kubernetesPod(w http.ResponseWriter, r *http.Request) {
	value, err := s.kubernetes.Pod(r.Context(), runtimeContext(r), r.PathValue("namespace"), r.PathValue("pod"))
	writeRuntimeResult(w, value, err)
}

func (s *Server) kubernetesPodLogs(w http.ResponseWriter, r *http.Request) {
	tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	output, err := s.kubernetes.Logs(
		r.Context(),
		runtimeContext(r),
		r.PathValue("namespace"),
		r.PathValue("pod"),
		r.URL.Query().Get("container"),
		queryBool(r, "previous"),
		tail,
	)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(output)
}

func (s *Server) kubernetesPodExec(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Container string   `json:"container"`
		Command   []string `json:"command"`
		Stdin     string   `json:"stdin"`
	}
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	output, err := s.kubernetes.Exec(
		r.Context(),
		runtimeContext(r),
		r.PathValue("namespace"),
		r.PathValue("pod"),
		request.Container,
		request.Command,
		[]byte(request.Stdin),
	)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"output": string(output)})
}

func (s *Server) kubernetesPodDebug(w http.ResponseWriter, r *http.Request) {
	if !podTerminalSupported() {
		http.Error(w, "interactive pod debug toolboxes require a PTY-capable host", http.StatusNotImplemented)
		return
	}
	var request struct {
		TargetContainer string `json:"targetContainer"`
		PodUID          string `json:"podUID"`
	}
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	request.TargetContainer = strings.TrimSpace(request.TargetContainer)
	request.PodUID = strings.TrimSpace(request.PodUID)
	if request.PodUID == "" {
		http.Error(w, "podUID is required", http.StatusBadRequest)
		return
	}
	if request.TargetContainer == "" {
		http.Error(w, "targetContainer is required", http.StatusBadRequest)
		return
	}
	value, err := s.kubernetes.StartDebugContainer(
		r.Context(),
		runtimeContext(r),
		r.PathValue("namespace"),
		r.PathValue("pod"),
		request.PodUID,
		request.TargetContainer,
	)
	writeRuntimeResult(w, value, err)
}

func (s *Server) kubernetesPodDebugStatus(w http.ResponseWriter, r *http.Request) {
	value, err := s.kubernetes.DebugContainer(
		r.Context(),
		runtimeContext(r),
		r.PathValue("namespace"),
		r.PathValue("pod"),
		r.URL.Query().Get("uid"),
		r.PathValue("container"),
	)
	writeRuntimeResult(w, value, err)
}

func (s *Server) kubernetesPodCapabilities(w http.ResponseWriter, r *http.Request) {
	value, err := s.kubernetes.ContainerCapabilities(
		r.Context(),
		runtimeContext(r),
		r.PathValue("namespace"),
		r.PathValue("pod"),
		r.URL.Query().Get("container"),
		queryBool(r, "files"),
	)
	writeRuntimeResult(w, value, err)
}

func (s *Server) kubernetesPodFiles(w http.ResponseWriter, r *http.Request) {
	value, err := s.kubernetes.Files(
		r.Context(),
		runtimeContext(r),
		r.PathValue("namespace"),
		r.PathValue("pod"),
		r.URL.Query().Get("container"),
		r.URL.Query().Get("path"),
	)
	writeRuntimeResult(w, value, err)
}

func (s *Server) kubernetesPodFile(w http.ResponseWriter, r *http.Request) {
	value, err := s.kubernetes.ReadFile(
		r.Context(),
		runtimeContext(r),
		r.PathValue("namespace"),
		r.PathValue("pod"),
		r.URL.Query().Get("container"),
		r.URL.Query().Get("path"),
	)
	writeRuntimeResult(w, value, err)
}

func (s *Server) kubernetesWritePodFile(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Container string `json:"container"`
		Path      string `json:"path"`
		Content   string `json:"content"`
	}
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	if err := s.kubernetes.WriteFile(
		r.Context(),
		runtimeContext(r),
		r.PathValue("namespace"),
		r.PathValue("pod"),
		request.Container,
		request.Path,
		[]byte(request.Content),
	); err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "saved"})
}

func (s *Server) kubernetesDeletePodFile(w http.ResponseWriter, r *http.Request) {
	if !queryBool(r, "confirm") {
		http.Error(w, "confirm=true is required to delete a pod file", http.StatusBadRequest)
		return
	}
	if err := s.kubernetes.DeleteFile(
		r.Context(),
		runtimeContext(r),
		r.PathValue("namespace"),
		r.PathValue("pod"),
		r.URL.Query().Get("container"),
		r.URL.Query().Get("path"),
	); err != nil {
		writeRuntimeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) kubernetesPodStats(w http.ResponseWriter, r *http.Request) {
	contextName := runtimeContext(r)
	value, err := s.kubernetes.Stats(
		r.Context(),
		contextName,
		r.PathValue("namespace"),
		r.PathValue("pod"),
	)
	if errors.Is(err, kubernetes.ErrMetricsUnavailable) {
		handled, installErr := s.clusters.EnsureMetricsServer(r.Context(), contextName)
		if handled && installErr == nil {
			value, err = s.kubernetes.Stats(
				r.Context(),
				contextName,
				r.PathValue("namespace"),
				r.PathValue("pod"),
			)
		} else if installErr != nil {
			err = errors.Join(err, installErr)
		}
	}
	writeRuntimeResult(w, value, err)
}

func (s *Server) kubernetesPodEvents(w http.ResponseWriter, r *http.Request) {
	value, err := s.kubernetes.Events(
		r.Context(),
		runtimeContext(r),
		r.PathValue("namespace"),
		r.PathValue("pod"),
	)
	writeRuntimeResult(w, value, err)
}

func (s *Server) kubernetesPodManifest(w http.ResponseWriter, r *http.Request) {
	output, err := s.kubernetes.Manifest(
		r.Context(),
		runtimeContext(r),
		r.PathValue("namespace"),
		r.PathValue("pod"),
	)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	_, _ = w.Write(output)
}

func (s *Server) createKubernetesCluster(w http.ResponseWriter, r *http.Request) {
	if !s.beginRuntimeOperation() {
		http.Error(w, "Porto is shutting down; cluster creation was not started", http.StatusServiceUnavailable)
		return
	}
	defer s.endRuntimeOperation()
	var request kubernetes.ClusterRequest
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	operationBase := s.runtimeContext
	if operationBase == nil {
		operationBase = context.Background()
	}
	operationContext, cancel := context.WithTimeout(operationBase, 20*time.Minute)
	defer cancel()
	release, err := s.beginKubernetesClusterOperation(operationContext, request.Name, "create")
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	defer release()
	cluster, err := s.clusters.Create(operationContext, request)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	s.rememberKubernetesClusterAddons(cluster.Context)
	writeJSONStatus(w, http.StatusCreated, cluster)
}

func (s *Server) kubernetesClusters(w http.ResponseWriter, r *http.Request) {
	value, err := s.clusters.List(r.Context())
	writeRuntimeResult(w, value, err)
}

func (s *Server) startKubernetesCluster(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	release, err := s.beginKubernetesClusterOperation(r.Context(), name, "start")
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	defer release()
	recreated, err := s.clusters.Start(r.Context(), name)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	if contextName, err := s.clusters.ContextName(name); err == nil {
		s.rememberKubernetesClusterAddons(contextName)
	}
	response := map[string]string{"status": "started"}
	if recreated {
		response["status"] = "recreated"
		response["message"] = "The control-plane container was missing, so Porto recreated the KinD cluster before starting it."
	}
	writeJSON(w, response)
}

func (s *Server) stopKubernetesCluster(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	release, err := s.beginKubernetesClusterOperation(r.Context(), name, "stop")
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	defer release()
	contextName, _ := s.clusters.ContextName(name)
	forwardErr := s.stopKubernetesClusterForwards(name)
	operationContext, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Minute)
	defer cancel()
	if err := errors.Join(forwardErr, s.clusters.SetRunning(operationContext, name, false)); err != nil {
		writeRuntimeError(w, err)
		return
	}
	s.forgetKubernetesClusterAddons("porto-"+name, contextName)
	writeJSON(w, map[string]string{"status": "stopped"})
}

func (s *Server) renameKubernetesCluster(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name string `json:"name"`
	}
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	oldName := r.PathValue("name")
	newName := strings.TrimSpace(request.Name)
	release, err := s.beginKubernetesClusterOperation(r.Context(), oldName, "rename", newName)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	defer release()
	oldContext, err := s.clusters.ContextName(oldName)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	if err := s.stopKubernetesClusterForwards(oldName); err != nil {
		writeRuntimeError(w, err)
		return
	}
	if err := s.clusters.Rename(r.Context(), oldName, newName); err != nil {
		writeRuntimeError(w, err)
		return
	}
	newContext, err := s.clusters.ContextName(newName)
	if err != nil {
		rollbackErr := s.clusters.Rename(r.Context(), newName, oldName)
		writeRuntimeError(w, errors.Join(err, renameRollbackError(rollbackErr)))
		return
	}
	if err := s.store.RenameKubernetesRoutesContext(r.Context(), oldContext, newContext); err != nil {
		rollbackErr := s.clusters.Rename(r.Context(), newName, oldName)
		writeRuntimeError(w, errors.Join(err, renameRollbackError(rollbackErr)))
		return
	}
	s.forgetKubernetesClusterAddons(oldContext)
	s.rememberKubernetesClusterAddons(newContext)
	writeJSON(w, map[string]string{"status": "renamed", "name": newName, "context": newContext})
}

func (s *Server) scaleKubernetesNodeGroup(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Version string                 `json:"version"`
		Count   int                    `json:"count"`
		Machine kubernetes.MachineSpec `json:"machine"`
		Labels  map[string]string      `json:"labels"`
		Taints  []string               `json:"taints"`
	}
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	group := kubernetes.NodeGroupSpec{
		Name:    r.PathValue("group"),
		Count:   request.Count,
		Machine: request.Machine,
		Labels:  request.Labels,
		Taints:  request.Taints,
	}
	release, err := s.beginKubernetesClusterOperation(r.Context(), r.PathValue("name"), "scale")
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	defer release()
	if err := s.clusters.ScaleNodeGroup(r.Context(), r.PathValue("name"), group, request.Version); err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "scaled"})
}

func (s *Server) importKubernetesImage(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Image string `json:"image"`
	}
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	release, err := s.beginKubernetesClusterOperation(r.Context(), r.PathValue("name"), "import")
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	defer release()
	if err := s.clusters.ImportImage(r.Context(), r.PathValue("name"), request.Image); err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "imported", "image": request.Image})
}

func (s *Server) deleteKubernetesCluster(w http.ResponseWriter, r *http.Request) {
	if !queryBool(r, "confirm") {
		http.Error(w, "confirm=true is required to delete a Kubernetes cluster", http.StatusBadRequest)
		return
	}
	name := r.PathValue("name")
	release, err := s.beginKubernetesClusterOperation(r.Context(), name, "delete")
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	defer release()
	contextName, contextErr := s.clusters.ContextName(name)
	forwardErr := s.stopKubernetesClusterForwards(name)
	operationContext, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Minute)
	defer cancel()
	httpRouteErr := s.deleteKubernetesHTTPRoutes(operationContext, contextName)
	deleteErr := s.clusters.Delete(operationContext, name)
	routeErr := s.deleteKubernetesClusterRoutes(operationContext, name, contextName)
	s.forgetKubernetesClusterAddons("porto-"+name, contextName)
	if httpRouteErr != nil && deleteErr == nil {
		log.Printf("delete Kubernetes HTTPRoutes for removed cluster %s: %v", name, httpRouteErr)
		httpRouteErr = nil
	}
	if err := errors.Join(contextErr, forwardErr, httpRouteErr, deleteErr, routeErr); err != nil {
		writeRuntimeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) beginKubernetesClusterOperation(
	ctx context.Context,
	clusterName string,
	action string,
	relatedNames ...string,
) (func(), error) {
	operationKeys, err := s.kubernetesClusterOperationKeys(append([]string{clusterName}, relatedNames...)...)
	if err != nil {
		return nil, err
	}
	s.kubeOperationMu.Lock()
	if s.kubeOperations == nil {
		s.kubeOperations = map[string]string{}
	}
	for _, operationKey := range operationKeys {
		if active := s.kubeOperations[operationKey]; active != "" {
			s.kubeOperationMu.Unlock()
			return nil, fmt.Errorf(
				"Kubernetes cluster %s operation %s is already in progress",
				clusterName,
				active,
			)
		}
	}
	gates := make([]chan struct{}, 0, len(operationKeys))
	for _, operationKey := range operationKeys {
		s.kubeOperations[operationKey] = action
		gates = append(gates, s.kubernetesClusterOperationGateLocked(operationKey))
	}
	s.kubeOperationMu.Unlock()
	acquired := 0
	for _, gate := range gates {
		select {
		case <-ctx.Done():
			releaseKubernetesOperationGates(gates[:acquired])
			s.kubeOperationMu.Lock()
			for _, operationKey := range operationKeys {
				delete(s.kubeOperations, operationKey)
			}
			s.kubeOperationMu.Unlock()
			return nil, ctx.Err()
		case <-gate:
			acquired++
		}
	}
	return func() {
		releaseKubernetesOperationGates(gates)
		s.kubeOperationMu.Lock()
		for _, operationKey := range operationKeys {
			delete(s.kubeOperations, operationKey)
		}
		s.kubeOperationMu.Unlock()
	}, nil
}

func (s *Server) tryBeginKubernetesClusterReconcile(
	ctx context.Context,
	clusterName string,
) (func(), bool, error) {
	operationKeys, err := s.kubernetesClusterOperationKeys(clusterName)
	if err != nil {
		return nil, false, err
	}
	s.kubeOperationMu.Lock()
	for _, operationKey := range operationKeys {
		if s.kubeOperations[operationKey] != "" {
			s.kubeOperationMu.Unlock()
			return nil, false, nil
		}
	}
	gates := make([]chan struct{}, 0, len(operationKeys))
	for _, operationKey := range operationKeys {
		gates = append(gates, s.kubernetesClusterOperationGateLocked(operationKey))
	}
	s.kubeOperationMu.Unlock()
	acquired := 0
	for _, gate := range gates {
		select {
		case <-ctx.Done():
			releaseKubernetesOperationGates(gates[:acquired])
			return nil, false, ctx.Err()
		case <-gate:
			acquired++
		}
	}
	s.kubeOperationMu.Lock()
	active := false
	for _, operationKey := range operationKeys {
		if s.kubeOperations[operationKey] != "" {
			active = true
			break
		}
	}
	s.kubeOperationMu.Unlock()
	if active {
		releaseKubernetesOperationGates(gates)
		return nil, false, nil
	}
	return func() {
		releaseKubernetesOperationGates(gates)
	}, true, nil
}

func (s *Server) kubernetesClusterOperationKeys(clusterNames ...string) ([]string, error) {
	keySet := make(map[string]struct{}, len(clusterNames)*2)
	for _, clusterName := range clusterNames {
		operationKey := clusterName
		if s.clusters != nil {
			var err error
			operationKey, err = s.clusters.OperationKey(clusterName)
			if err != nil {
				return nil, err
			}
		}
		keySet["name:"+clusterName] = struct{}{}
		keySet["runtime:"+operationKey] = struct{}{}
	}
	keys := make([]string, 0, len(keySet))
	for key := range keySet {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, nil
}

func (s *Server) kubernetesClusterOperationGateLocked(operationKey string) chan struct{} {
	if s.kubeOpGates == nil {
		s.kubeOpGates = map[string]chan struct{}{}
	}
	gate := s.kubeOpGates[operationKey]
	if gate == nil {
		gate = make(chan struct{}, 1)
		gate <- struct{}{}
		s.kubeOpGates[operationKey] = gate
	}
	return gate
}

func releaseKubernetesOperationGates(gates []chan struct{}) {
	for index := len(gates) - 1; index >= 0; index-- {
		gates[index] <- struct{}{}
	}
}

func renameRollbackError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("roll back Kubernetes cluster rename: %w", err)
}

func (s *Server) stopKubernetesClusterForwards(clusterName string) error {
	legacyContext := "porto-" + clusterName
	stopErrors := []error{s.stopKubernetesForwards(legacyContext)}
	contextName, err := s.clusters.ContextName(clusterName)
	if err != nil {
		stopErrors = append(stopErrors, err)
	} else if contextName != legacyContext {
		stopErrors = append(stopErrors, s.stopKubernetesForwards(contextName))
	}
	return errors.Join(stopErrors...)
}

func (s *Server) vmStatus(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.Settings(r.Context())
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	if !settings.VMsEnabled {
		writeJSON(w, vm.Status{Provider: "lima", Message: "VM runtime is disabled"})
		return
	}
	status := s.vms.Status(r.Context())
	status.Enabled = true
	writeJSON(w, status)
}

func (s *Server) vmImages(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.vms.Images())
}

func (s *Server) vmInstances(w http.ResponseWriter, r *http.Request) {
	value, err := s.vms.List(r.Context())
	writeRuntimeResult(w, value, err)
}

func (s *Server) createVM(w http.ResponseWriter, r *http.Request) {
	var request vm.CreateRequest
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	instance, err := s.vms.Create(r.Context(), request)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, instance)
}

func (s *Server) startVM(w http.ResponseWriter, r *http.Request) {
	if !s.requireStandaloneVM(w, r.PathValue("name")) {
		return
	}
	if err := s.vms.Start(r.Context(), r.PathValue("name")); err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "started"})
}

func (s *Server) stopVM(w http.ResponseWriter, r *http.Request) {
	if !s.requireStandaloneVM(w, r.PathValue("name")) {
		return
	}
	if err := s.vms.Stop(r.Context(), r.PathValue("name")); err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "stopped"})
}

func (s *Server) execVM(w http.ResponseWriter, r *http.Request) {
	if !s.requireStandaloneVM(w, r.PathValue("name")) {
		return
	}
	var request struct {
		Command []string `json:"command"`
		Stdin   string   `json:"stdin"`
	}
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	output, err := s.vms.Exec(r.Context(), r.PathValue("name"), request.Command, []byte(request.Stdin))
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"output": string(output)})
}

func (s *Server) snapshotVM(w http.ResponseWriter, r *http.Request) {
	if !s.requireStandaloneVM(w, r.PathValue("name")) {
		return
	}
	var request struct {
		Name string `json:"name"`
	}
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	if err := s.vms.CreateSnapshot(r.Context(), r.PathValue("name"), request.Name); err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, map[string]string{"status": "created"})
}

func (s *Server) restoreVMSnapshot(w http.ResponseWriter, r *http.Request) {
	if !s.requireStandaloneVM(w, r.PathValue("name")) {
		return
	}
	var request struct {
		Name string `json:"name"`
	}
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	if err := s.vms.RestoreSnapshot(r.Context(), r.PathValue("name"), request.Name); err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "restored"})
}

func (s *Server) deleteVM(w http.ResponseWriter, r *http.Request) {
	if !queryBool(r, "confirm") {
		http.Error(w, "confirm=true is required to delete a VM", http.StatusBadRequest)
		return
	}
	if !s.requireStandaloneVM(w, r.PathValue("name")) {
		return
	}
	if err := s.vms.Delete(r.Context(), r.PathValue("name"), queryBool(r, "force")); err != nil {
		writeRuntimeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decodeRuntimeJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRuntimeRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		http.Error(w, "request must contain one JSON object", http.StatusBadRequest)
		return false
	}
	return true
}

func writeRuntimeResult[T any](w http.ResponseWriter, value T, err error) {
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, value)
}

func writeRuntimeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "unavailable"),
		strings.Contains(message, "not found"),
		strings.Contains(message, "connection refused"),
		strings.Contains(message, "no upstream"):
		status = http.StatusServiceUnavailable
	case strings.Contains(message, "required"),
		strings.Contains(message, "invalid"),
		strings.Contains(message, "unsupported"),
		strings.Contains(message, "refusing"):
		status = http.StatusBadRequest
	}
	writeJSONStatus(w, status, map[string]string{"message": err.Error()})
}

func runtimeContext(r *http.Request) string {
	return r.URL.Query().Get("context")
}

func queryBool(r *http.Request, name string) bool {
	value := strings.TrimSpace(strings.ToLower(r.URL.Query().Get(name)))
	return value == "1" || value == "true" || value == "yes"
}

func (s *Server) runtimeFeatures(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.Settings(r.Context())
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, runtimeFeatureSnapshot(settings))
}

func (s *Server) setRuntimeFeature(w http.ResponseWriter, r *http.Request) {
	feature := r.PathValue("feature")
	action := r.PathValue("action")
	if action != "enable" && action != "disable" {
		http.Error(w, "runtime feature action must be enable or disable", http.StatusBadRequest)
		return
	}
	settings, err := s.store.Settings(r.Context())
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	enabled := action == "enable"
	switch feature {
	case "docker":
		settings.DockerEnabled = enabled
	case "kubernetes":
		settings.KubernetesEnabled = enabled
	case "vms":
		settings.VMsEnabled = enabled
	default:
		http.Error(w, "unknown runtime feature", http.StatusNotFound)
		return
	}

	if feature == "docker" && !enabled {
		if s.hasRuntimeOperations() {
			http.Error(w, "wait for active runtime operations to finish before disabling Docker", http.StatusConflict)
			return
		}
		statePath, pathErr := config.DockerEndpointStatePath()
		if pathErr != nil {
			writeRuntimeError(w, pathErr)
			return
		}
		if _, statErr := os.Stat(statePath); statErr == nil {
			http.Error(w, "deactivate the canonical Docker endpoint before disabling Docker", http.StatusConflict)
			return
		} else if !errors.Is(statErr, os.ErrNotExist) {
			writeRuntimeError(w, statErr)
			return
		}
	}

	var rollback func() error
	if feature == "docker" {
		if enabled {
			if err := s.startDockerAPI(s.runtimeContext); err != nil {
				writeRuntimeError(w, err)
				return
			}
			rollback = func() error {
				rollbackContext, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
				defer cancel()
				return s.stopDockerAPI(rollbackContext)
			}
		} else {
			stopContext, cancel := context.WithTimeout(r.Context(), httpShutdownTimeout)
			err := s.stopDockerAPI(stopContext)
			cancel()
			if err != nil {
				writeRuntimeError(w, err)
				return
			}
			rollback = func() error {
				return s.startDockerAPI(s.runtimeContext)
			}
		}
	}
	if err := s.store.SetSettings(r.Context(), settings); err != nil {
		if rollback != nil {
			err = errors.Join(err, rollback())
		}
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, runtimeFeatureSnapshot(settings))
}

func (s *Server) requireRuntime(feature string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		settings, err := s.store.Settings(r.Context())
		if err != nil {
			writeRuntimeError(w, err)
			return
		}
		if !runtimeFeatureEnabled(settings, feature) {
			writeJSONStatus(w, http.StatusConflict, map[string]string{
				"message": feature + " runtime is disabled; enable it in Settings or with the Porto CLI",
			})
			return
		}
		next(w, r)
	}
}

func runtimeFeatureEnabled(settings app.Settings, feature string) bool {
	switch feature {
	case "docker":
		return settings.DockerEnabled
	case "kubernetes":
		return settings.KubernetesEnabled
	case "vms":
		return settings.VMsEnabled
	default:
		return false
	}
}

func runtimeFeatureSnapshot(settings app.Settings) map[string]bool {
	return map[string]bool{
		"docker":     settings.DockerEnabled,
		"kubernetes": settings.KubernetesEnabled,
		"vms":        settings.VMsEnabled,
	}
}

func (s *Server) dockerEndpointStatus(status portodocker.Status) portodocker.Status {
	statePath, err := config.DockerEndpointStatePath()
	if err != nil {
		if status.Message == "" {
			status.Message = err.Error()
		}
		return status
	}
	return portodocker.AddEndpointStatus(status, config.CanonicalDockerSocketPath(), statePath)
}

func (s *Server) requireStandaloneVM(w http.ResponseWriter, name string) bool {
	if err := s.vms.EnsureStandalone(name); err != nil {
		writeRuntimeError(w, err)
		return false
	}
	return true
}
