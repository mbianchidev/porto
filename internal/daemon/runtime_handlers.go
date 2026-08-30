package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
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
	mux.HandleFunc("GET /api/docker/status", s.dockerStatus)
	mux.HandleFunc("GET /api/docker/containers", s.requireRuntime("docker", s.dockerContainers))
	mux.HandleFunc("GET /api/docker/containers/stats", s.requireRuntime("docker", s.dockerContainerStats))
	mux.HandleFunc("GET /api/docker/containers/{id}", s.requireRuntime("docker", s.dockerContainer))
	mux.HandleFunc("GET /api/docker/containers/{id}/logs", s.requireRuntime("docker", s.dockerContainerLogs))
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
	mux.HandleFunc("GET /api/kubernetes/nodes", s.requireRuntime("kubernetes", s.kubernetesNodes))
	mux.HandleFunc("GET /api/kubernetes/pods/{namespace}/{pod}", s.requireRuntime("kubernetes", s.kubernetesPod))
	mux.HandleFunc("GET /api/kubernetes/pods/{namespace}/{pod}/logs", s.requireRuntime("kubernetes", s.kubernetesPodLogs))
	mux.HandleFunc("POST /api/kubernetes/pods/{namespace}/{pod}/exec", s.requireRuntime("kubernetes", s.kubernetesPodExec))
	mux.HandleFunc("GET /api/kubernetes/pods/{namespace}/{pod}/terminal", s.requireRuntime("kubernetes", s.kubernetesPodTerminal))
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
	mux.HandleFunc("POST /api/kubernetes/clusters/{name}/node-groups/{group}", s.requireRuntime("kubernetes", s.scaleKubernetesNodeGroup))
	mux.HandleFunc("POST /api/kubernetes/clusters/{name}/images/import", s.requireRuntime("kubernetes", s.importKubernetesImage))
	mux.HandleFunc("DELETE /api/kubernetes/clusters/{name}", s.requireRuntime("kubernetes", s.deleteKubernetesCluster))

	mux.HandleFunc("GET /api/vms/status", s.vmStatus)
	mux.HandleFunc("GET /api/vms/images", s.vmImages)
	mux.HandleFunc("GET /api/vms/instances", s.requireRuntime("vms", s.vmInstances))
	mux.HandleFunc("POST /api/vms/instances", s.requireRuntime("vms", s.createVM))
	mux.HandleFunc("POST /api/vms/instances/{name}/start", s.requireRuntime("vms", s.startVM))
	mux.HandleFunc("POST /api/vms/instances/{name}/stop", s.requireRuntime("vms", s.stopVM))
	mux.HandleFunc("POST /api/vms/instances/{name}/exec", s.requireRuntime("vms", s.execVM))
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

func (s *Server) dockerContainers(w http.ResponseWriter, r *http.Request) {
	value, err := s.docker.Containers(r.Context())
	writeRuntimeResult(w, value, err)
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
	if err := s.docker.ContainerAction(r.Context(), r.PathValue("id"), r.PathValue("action")); err != nil {
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
	if err := s.docker.PullImage(r.Context(), request.Reference); err != nil {
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
	if err := s.docker.CreateNetwork(r.Context(), request); err != nil {
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
	if err := s.docker.CreateVolume(r.Context(), request.Name, request.Driver); err != nil {
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
	value, err := s.kubernetes.Services(r.Context(), runtimeContext(r), r.URL.Query().Get("namespace"))
	writeRuntimeResult(w, value, err)
}

func (s *Server) kubernetesNodes(w http.ResponseWriter, r *http.Request) {
	value, err := s.kubernetes.Nodes(r.Context(), runtimeContext(r))
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
	value, err := s.kubernetes.Stats(
		r.Context(),
		runtimeContext(r),
		r.PathValue("namespace"),
		r.PathValue("pod"),
	)
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
	var request kubernetes.ClusterRequest
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	cluster, err := s.clusters.Create(r.Context(), request)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, cluster)
}

func (s *Server) kubernetesClusters(w http.ResponseWriter, r *http.Request) {
	value, err := s.clusters.List(r.Context())
	writeRuntimeResult(w, value, err)
}

func (s *Server) startKubernetesCluster(w http.ResponseWriter, r *http.Request) {
	if err := s.clusters.SetRunning(r.Context(), r.PathValue("name"), true); err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "started"})
}

func (s *Server) stopKubernetesCluster(w http.ResponseWriter, r *http.Request) {
	if err := s.clusters.SetRunning(r.Context(), r.PathValue("name"), false); err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "stopped"})
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
	if err := s.clusters.Delete(r.Context(), r.PathValue("name")); err != nil {
		writeRuntimeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
	if err := s.vms.Start(r.Context(), r.PathValue("name")); err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "started"})
}

func (s *Server) stopVM(w http.ResponseWriter, r *http.Request) {
	if err := s.vms.Stop(r.Context(), r.PathValue("name")); err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "stopped"})
}

func (s *Server) execVM(w http.ResponseWriter, r *http.Request) {
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

	if feature == "docker" {
		if enabled {
			runContext := s.runtimeContext
			if runContext == nil {
				runContext = context.Background()
			}
			if err := s.startDockerProxy(runContext); err != nil {
				writeRuntimeError(w, err)
				return
			}
		} else {
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
			stopContext, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			if err := s.stopDockerProxy(stopContext); err != nil {
				writeRuntimeError(w, err)
				return
			}
		}
	}

	if err := s.store.SetSettings(r.Context(), settings); err != nil {
		if feature == "docker" && enabled {
			stopContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			stopErr := s.stopDockerProxy(stopContext)
			cancel()
			writeRuntimeError(w, errors.Join(err, stopErr))
			return
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
