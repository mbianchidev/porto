package daemon

import (
	"encoding/json"
	"net/http"
)

// The native worker reuses resource contracts without registering projects,
// user settings, Linux data helpers, Kubernetes or VM handlers.
func (s *Server) dockerResourceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/docker/status", s.dockerStatus)
	if !s.windowsWorker {
		mux.HandleFunc("GET /api/docker/cleanup", s.dockerCleanupStatus)
		mux.HandleFunc("GET /api/docker/engine/resources", s.engineResourceStatus)
		mux.HandleFunc("PUT /api/docker/engine/resources", s.requireRuntime("docker", s.configureEngineResources))
		mux.HandleFunc("POST /api/docker/engine/resources/apply", s.requireRuntime("docker", s.applyEngineResources))
		mux.HandleFunc("POST /api/docker/cleanup", s.requireRuntime("docker", s.runDockerCleanupNow))
		mux.HandleFunc("POST /api/docker/engine/install", s.requireRuntime("docker", s.installDockerEngine))
		mux.HandleFunc("POST /api/docker/engine/stop", s.requireRuntime("docker", s.stopDockerEngine))
		mux.HandleFunc("POST /api/docker/engine/remove", s.requireRuntime("docker", s.removeDockerEngine))
		mux.HandleFunc("POST /api/docker/engine/prepare-update", s.prepareDockerEngineUpdate)
		mux.HandleFunc("POST /api/docker/context/install", s.requireRuntime("docker", s.installDockerContext))
	}
	mux.HandleFunc("GET /api/docker/containers", s.requireRuntime("docker", s.dockerContainers))
	mux.HandleFunc("POST /api/docker/containers", s.requireRuntime("docker", s.createDockerContainer))
	mux.HandleFunc("GET /api/docker/containers/snapshot", s.requireRuntime("docker", s.dockerContainerSnapshot))
	mux.HandleFunc("GET /api/docker/containers/events", s.requireRuntime("docker", s.dockerContainerEvents))
	mux.HandleFunc("GET /api/docker/containers/stats", s.requireRuntime("docker", s.dockerContainerStats))
	mux.HandleFunc("GET /api/docker/containers/{id}", s.requireRuntime("docker", s.dockerContainer))
	mux.HandleFunc("GET /api/docker/containers/{id}/identity", s.requireRuntime("docker", s.dockerContainerIdentity))
	mux.HandleFunc("GET /api/docker/containers/{id}/logs", s.requireRuntime("docker", s.dockerContainerLogs))
	mux.HandleFunc("GET /api/docker/containers/{id}/terminal", s.requireRuntime("docker", s.dockerContainerTerminal))
	mux.HandleFunc("POST /api/docker/containers/{id}/exec", s.requireRuntime("docker", s.dockerContainerExec))
	mux.HandleFunc("POST /api/docker/containers/{id}/{action}", s.requireRuntime("docker", s.dockerContainerAction))
	mux.HandleFunc("GET /api/docker/images", s.requireRuntime("docker", s.dockerImages))
	if !s.windowsWorker {
		mux.HandleFunc("GET /api/docker/images/{id}/dive", s.requireRuntime("docker", s.dockerImageDiveTerminal))
	}

	mux.HandleFunc("GET /api/docker/images/{id}", s.requireRuntime("docker", s.dockerImage))
	mux.HandleFunc("POST /api/docker/images/pull", s.requireRuntime("docker", s.dockerPullImage))
	mux.HandleFunc("DELETE /api/docker/images/{id}", s.requireRuntime("docker", s.dockerRemoveImage))
	if !s.windowsWorker {
		mux.HandleFunc("GET /api/docker/builds", s.requireRuntime("docker", s.dockerBuilds))
		mux.HandleFunc("POST /api/docker/builds", s.requireRuntime("docker", s.dockerBuild))
	}
	mux.HandleFunc("GET /api/docker/networks", s.requireRuntime("docker", s.dockerNetworks))
	mux.HandleFunc("GET /api/docker/networks/{name}", s.requireRuntime("docker", s.dockerNetwork))
	mux.HandleFunc("POST /api/docker/networks", s.requireRuntime("docker", s.dockerCreateNetwork))
	mux.HandleFunc("DELETE /api/docker/networks/{name}", s.requireRuntime("docker", s.dockerRemoveNetwork))
	mux.HandleFunc("GET /api/docker/volumes", s.requireRuntime("docker", s.dockerVolumes))
	mux.HandleFunc("GET /api/docker/volumes/{name}", s.requireRuntime("docker", s.dockerVolume))
	mux.HandleFunc("POST /api/docker/volumes", s.requireRuntime("docker", s.dockerCreateVolume))
	mux.HandleFunc("DELETE /api/docker/volumes/{name}", s.requireRuntime("docker", s.dockerRemoveVolume))
}

func (s *Server) dockerContainerIdentity(w http.ResponseWriter, r *http.Request) {
	document, err := s.docker.InspectContainer(r.Context(), r.PathValue("id"))
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	var value struct {
		ID    string `json:"Id"`
		State struct {
			Status  string
			Running bool
			Paused  bool
		}
	}
	if err := json.Unmarshal(document, &value); err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, value)
}
