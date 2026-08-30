package daemon

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	portodocker "github.com/mbianchidev/porto/internal/docker"
	"github.com/mbianchidev/porto/internal/kubernetes"
	"github.com/mbianchidev/porto/internal/vm"
)

const maxRuntimeRequestBytes = 2 * 1024 * 1024

func (s *Server) runtimeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/runtime", s.runtimeStatus)
	mux.HandleFunc("GET /api/docker/status", s.dockerStatus)
	mux.HandleFunc("GET /api/docker/containers", s.dockerContainers)
	mux.HandleFunc("POST /api/docker/containers/{id}/{action}", s.dockerContainerAction)
	mux.HandleFunc("GET /api/docker/images", s.dockerImages)
	mux.HandleFunc("POST /api/docker/images/pull", s.dockerPullImage)
	mux.HandleFunc("DELETE /api/docker/images/{id}", s.dockerRemoveImage)
	mux.HandleFunc("GET /api/docker/builds", s.dockerBuilds)
	mux.HandleFunc("POST /api/docker/builds", s.dockerBuild)
	mux.HandleFunc("GET /api/docker/networks", s.dockerNetworks)
	mux.HandleFunc("POST /api/docker/networks", s.dockerCreateNetwork)
	mux.HandleFunc("DELETE /api/docker/networks/{name}", s.dockerRemoveNetwork)
	mux.HandleFunc("GET /api/docker/volumes", s.dockerVolumes)
	mux.HandleFunc("POST /api/docker/volumes", s.dockerCreateVolume)
	mux.HandleFunc("DELETE /api/docker/volumes/{name}", s.dockerRemoveVolume)

	mux.HandleFunc("GET /api/kubernetes/status", s.kubernetesStatus)
	mux.HandleFunc("GET /api/kubernetes/contexts", s.kubernetesContexts)
	mux.HandleFunc("GET /api/kubernetes/pods", s.kubernetesPods)
	mux.HandleFunc("GET /api/kubernetes/services", s.kubernetesServices)
	mux.HandleFunc("GET /api/kubernetes/nodes", s.kubernetesNodes)
	mux.HandleFunc("GET /api/kubernetes/pods/{namespace}/{pod}", s.kubernetesPod)
	mux.HandleFunc("GET /api/kubernetes/pods/{namespace}/{pod}/logs", s.kubernetesPodLogs)
	mux.HandleFunc("POST /api/kubernetes/pods/{namespace}/{pod}/exec", s.kubernetesPodExec)
	mux.HandleFunc("GET /api/kubernetes/pods/{namespace}/{pod}/files", s.kubernetesPodFiles)
	mux.HandleFunc("GET /api/kubernetes/pods/{namespace}/{pod}/file", s.kubernetesPodFile)
	mux.HandleFunc("PUT /api/kubernetes/pods/{namespace}/{pod}/file", s.kubernetesWritePodFile)
	mux.HandleFunc("DELETE /api/kubernetes/pods/{namespace}/{pod}/file", s.kubernetesDeletePodFile)
	mux.HandleFunc("GET /api/kubernetes/pods/{namespace}/{pod}/stats", s.kubernetesPodStats)
	mux.HandleFunc("GET /api/kubernetes/pods/{namespace}/{pod}/events", s.kubernetesPodEvents)
	mux.HandleFunc("GET /api/kubernetes/pods/{namespace}/{pod}/manifest", s.kubernetesPodManifest)
	mux.HandleFunc("GET /api/kubernetes/clusters", s.kubernetesClusters)
	mux.HandleFunc("POST /api/kubernetes/clusters", s.createKubernetesCluster)
	mux.HandleFunc("POST /api/kubernetes/clusters/{name}/start", s.startKubernetesCluster)
	mux.HandleFunc("POST /api/kubernetes/clusters/{name}/stop", s.stopKubernetesCluster)
	mux.HandleFunc("POST /api/kubernetes/clusters/{name}/node-groups/{group}", s.scaleKubernetesNodeGroup)
	mux.HandleFunc("DELETE /api/kubernetes/clusters/{name}", s.deleteKubernetesCluster)

	mux.HandleFunc("GET /api/vms/status", s.vmStatus)
	mux.HandleFunc("GET /api/vms/images", s.vmImages)
	mux.HandleFunc("GET /api/vms/instances", s.vmInstances)
	mux.HandleFunc("POST /api/vms/instances", s.createVM)
	mux.HandleFunc("POST /api/vms/instances/{name}/start", s.startVM)
	mux.HandleFunc("POST /api/vms/instances/{name}/stop", s.stopVM)
	mux.HandleFunc("POST /api/vms/instances/{name}/exec", s.execVM)
	mux.HandleFunc("POST /api/vms/instances/{name}/snapshot", s.snapshotVM)
	mux.HandleFunc("POST /api/vms/instances/{name}/restore", s.restoreVMSnapshot)
	mux.HandleFunc("DELETE /api/vms/instances/{name}", s.deleteVM)
}

func (s *Server) runtimeStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"docker":     s.docker.Status(r.Context(), s.dockerSocket),
		"kubernetes": s.kubernetes.Status(r.Context(), r.URL.Query().Get("context")),
		"vms":        s.vms.Status(r.Context()),
	})
}

func (s *Server) dockerStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.docker.Status(r.Context(), s.dockerSocket))
}

func (s *Server) dockerContainers(w http.ResponseWriter, r *http.Request) {
	value, err := s.docker.Containers(r.Context())
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
	writeJSON(w, s.kubernetes.Status(r.Context(), r.URL.Query().Get("context")))
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
	writeJSON(w, s.vms.Status(r.Context()))
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
