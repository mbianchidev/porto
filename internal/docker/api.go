package docker

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/runtimes"
)

const (
	dockerAPIVersion    = "1.47"
	dockerMinAPIVersion = "1.41"
)

type API struct {
	manager    *Manager
	socketPath string
	mux        *http.ServeMux
}

func NewAPI(manager *Manager, socketPath string) *API {
	api := &API{manager: manager, socketPath: socketPath, mux: http.NewServeMux()}
	api.routes()
	return api
}

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := stripAPIVersion(r.URL.Path)
	request := r.Clone(r.Context())
	request.URL.Path = path
	w.Header().Set("Api-Version", dockerAPIVersion)
	w.Header().Set("Docker-Experimental", "false")
	w.Header().Set("Ostype", "linux")
	w.Header().Set("Server", "Porto/"+config.Version)
	a.mux.ServeHTTP(w, request)
}

func (a *API) routes() {
	a.mux.HandleFunc("GET /_ping", a.ping)
	a.mux.HandleFunc("HEAD /_ping", a.ping)
	a.mux.HandleFunc("GET /version", a.version)
	a.mux.HandleFunc("GET /info", a.info)

	a.mux.HandleFunc("GET /containers/json", a.containers)
	a.mux.HandleFunc("POST /containers/create", a.createContainer)
	a.mux.HandleFunc("GET /containers/{id}/json", a.inspectContainer)
	a.mux.HandleFunc("POST /containers/{id}/start", a.containerAction("start"))
	a.mux.HandleFunc("POST /containers/{id}/stop", a.containerAction("stop"))
	a.mux.HandleFunc("POST /containers/{id}/restart", a.containerAction("restart"))
	a.mux.HandleFunc("POST /containers/{id}/pause", a.containerAction("pause"))
	a.mux.HandleFunc("POST /containers/{id}/unpause", a.containerAction("unpause"))
	a.mux.HandleFunc("POST /containers/{id}/rename", a.renameContainer)
	a.mux.HandleFunc("POST /containers/{id}/wait", a.waitContainer)
	a.mux.HandleFunc("GET /containers/{id}/logs", a.containerLogs)
	a.mux.HandleFunc("DELETE /containers/{id}", a.deleteContainer)

	a.mux.HandleFunc("GET /images/json", a.images)
	a.mux.HandleFunc("GET /images/{id}/json", a.inspectImage)
	a.mux.HandleFunc("POST /images/create", a.pullImage)
	a.mux.HandleFunc("DELETE /images/{id}", a.deleteImage)

	a.mux.HandleFunc("GET /networks", a.networks)
	a.mux.HandleFunc("POST /networks/create", a.createNetwork)
	a.mux.HandleFunc("GET /networks/{id}", a.inspectNetwork)
	a.mux.HandleFunc("DELETE /networks/{id}", a.deleteNetwork)

	a.mux.HandleFunc("GET /volumes", a.volumes)
	a.mux.HandleFunc("POST /volumes/create", a.createVolume)
	a.mux.HandleFunc("GET /volumes/{name}", a.inspectVolume)
	a.mux.HandleFunc("DELETE /volumes/{name}", a.deleteVolume)

	a.mux.HandleFunc("/", a.unsupported)
}

func (a *API) ping(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Builder-Version", "")
	if _, err := w.Write([]byte("OK")); err != nil {
		return
	}
}

func (a *API) version(w http.ResponseWriter, _ *http.Request) {
	writeDockerJSON(w, http.StatusOK, map[string]any{
		"Platform": map[string]string{"Name": "Porto Engine"},
		"Components": []map[string]any{{
			"Name":    "Engine",
			"Version": config.Version,
			"Details": map[string]string{
				"ApiVersion":    dockerAPIVersion,
				"MinAPIVersion": dockerMinAPIVersion,
				"Os":            "linux",
				"Arch":          runtime.GOARCH,
			},
		}},
		"Version":       config.Version,
		"ApiVersion":    dockerAPIVersion,
		"MinAPIVersion": dockerMinAPIVersion,
		"GitCommit":     "porto",
		"GoVersion":     runtime.Version(),
		"Os":            "linux",
		"Arch":          runtime.GOARCH,
		"KernelVersion": "",
		"BuildTime":     time.Now().UTC().Format(time.RFC3339),
		"Experimental":  false,
	})
}

func (a *API) info(w http.ResponseWriter, r *http.Request) {
	status := a.manager.Status(r.Context(), a.socketPath)
	containers, containerErr := a.manager.Containers(r.Context())
	images, imageErr := a.manager.Images(r.Context())
	warnings := make([]string, 0, 3)
	if status.Message != "" {
		warnings = append(warnings, status.Message)
	}
	if containerErr != nil && status.Available {
		warnings = append(warnings, containerErr.Error())
	}
	if imageErr != nil && status.Available {
		warnings = append(warnings, imageErr.Error())
	}
	running, paused, stopped := 0, 0, 0
	for _, container := range containers {
		switch strings.ToLower(container.State) {
		case "running", "up":
			running++
		case "paused":
			paused++
		default:
			stopped++
		}
	}
	driver := "porto-containerd"
	if status.Backend != "" {
		driver = status.Backend
	}
	writeDockerJSON(w, http.StatusOK, map[string]any{
		"ID":                "porto",
		"Containers":        len(containers),
		"ContainersRunning": running,
		"ContainersPaused":  paused,
		"ContainersStopped": stopped,
		"Images":            len(images),
		"Driver":            driver,
		"Plugins": map[string]any{
			"Volume":        []string{"local"},
			"Network":       []string{"bridge", "host", "none"},
			"Authorization": nil,
			"Log":           []string{"json-file"},
		},
		"MemoryLimit":        true,
		"SwapLimit":          true,
		"CpuCfsPeriod":       true,
		"CpuCfsQuota":        true,
		"CPUShares":          true,
		"CPUSet":             true,
		"PidsLimit":          true,
		"IPv4Forwarding":     true,
		"Debug":              false,
		"NFd":                0,
		"OomKillDisable":     true,
		"NGoroutines":        0,
		"SystemTime":         time.Now().UTC().Format(time.RFC3339Nano),
		"LoggingDriver":      "json-file",
		"CgroupDriver":       "systemd",
		"NEventsListener":    0,
		"KernelVersion":      "",
		"OperatingSystem":    "Porto Engine",
		"OSVersion":          config.Version,
		"OSType":             "linux",
		"Architecture":       runtime.GOARCH,
		"NCPU":               runtime.NumCPU(),
		"MemTotal":           0,
		"Name":               "porto",
		"ServerVersion":      config.Version,
		"DockerRootDir":      "porto://containerd",
		"IndexServerAddress": "https://index.docker.io/v1/",
		"RegistryConfig": map[string]any{
			"AllowNondistributableArtifactsCIDRs":     nil,
			"AllowNondistributableArtifactsHostnames": nil,
			"InsecureRegistryCIDRs":                   []string{"127.0.0.0/8"},
			"IndexConfigs":                            map[string]any{},
			"Mirrors":                                 nil,
		},
		"LiveRestoreEnabled": false,
		"ExperimentalBuild":  false,
		"Warnings":           warnings,
	})
}

func (a *API) containers(w http.ResponseWriter, r *http.Request) {
	for _, parameter := range []string{"limit", "since", "before", "size", "filters"} {
		if r.URL.Query().Get(parameter) != "" {
			writeDockerUnsupported(w, "container list query parameter "+parameter)
			return
		}
	}
	containers, err := a.manager.DockerContainers(r.Context(), dockerBool(r, "all"))
	if err != nil {
		writeDockerError(w, err)
		return
	}
	response := make([]map[string]any, 0, len(containers))
	for _, container := range containers {
		name := container.Name
		if name != "" && !strings.HasPrefix(name, "/") {
			name = "/" + name
		}
		response = append(response, map[string]any{
			"Id":      container.ID,
			"Names":   []string{name},
			"Image":   container.Image,
			"ImageID": container.ImageID,
			"Command": container.Command,
			"Created": parseDockerTime(container.CreatedAt),
			"Ports":   []any{},
			"Labels":  container.Labels,
			"State":   container.State,
			"Status":  container.Status,
			"HostConfig": map[string]any{
				"NetworkMode": firstNonEmpty(container.Networks, "default"),
			},
			"NetworkSettings": map[string]any{"Networks": map[string]any{}},
			"Mounts":          []any{},
		})
	}
	writeDockerJSON(w, http.StatusOK, response)
}

func (a *API) createContainer(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Hostname        string            `json:"Hostname"`
		User            string            `json:"User"`
		Env             []string          `json:"Env"`
		Cmd             []string          `json:"Cmd"`
		Image           string            `json:"Image"`
		WorkingDir      string            `json:"WorkingDir"`
		Entrypoint      []string          `json:"Entrypoint"`
		Labels          map[string]string `json:"Labels"`
		Tty             bool              `json:"Tty"`
		OpenStdin       bool              `json:"OpenStdin"`
		NetworkDisabled bool              `json:"NetworkDisabled"`
		AttachStdin     bool              `json:"AttachStdin"`
		AttachStdout    bool              `json:"AttachStdout"`
		AttachStderr    bool              `json:"AttachStderr"`
		HostConfig      struct {
			Binds        []string `json:"Binds"`
			Mounts       []any    `json:"Mounts"`
			AutoRemove   bool     `json:"AutoRemove"`
			NetworkMode  string   `json:"NetworkMode"`
			Privileged   bool     `json:"Privileged"`
			ReadonlyRoot bool     `json:"ReadonlyRootfs"`
			PortBindings map[string][]struct {
				HostIP   string `json:"HostIp"`
				HostPort string `json:"HostPort"`
			} `json:"PortBindings"`
			RestartPolicy struct {
				Name              string `json:"Name"`
				MaximumRetryCount int    `json:"MaximumRetryCount"`
			} `json:"RestartPolicy"`
			Devices            []any             `json:"Devices"`
			DeviceRequests     []any             `json:"DeviceRequests"`
			DeviceCgroupRules  []string          `json:"DeviceCgroupRules"`
			CapAdd             []string          `json:"CapAdd"`
			CapDrop            []string          `json:"CapDrop"`
			SecurityOpt        []string          `json:"SecurityOpt"`
			ExtraHosts         []string          `json:"ExtraHosts"`
			DNS                []string          `json:"Dns"`
			DNSOptions         []string          `json:"DnsOptions"`
			DNSSearch          []string          `json:"DnsSearch"`
			GroupAdd           []string          `json:"GroupAdd"`
			Ulimits            []any             `json:"Ulimits"`
			MaskedPaths        []string          `json:"MaskedPaths"`
			ReadonlyPaths      []string          `json:"ReadonlyPaths"`
			Sysctls            map[string]string `json:"Sysctls"`
			Tmpfs              map[string]string `json:"Tmpfs"`
			StorageOpt         map[string]string `json:"StorageOpt"`
			ShmSize            int64             `json:"ShmSize"`
			Runtime            string            `json:"Runtime"`
			Isolation          string            `json:"Isolation"`
			CgroupParent       string            `json:"CgroupParent"`
			Cgroup             string            `json:"Cgroup"`
			CgroupnsMode       string            `json:"CgroupnsMode"`
			UsernsMode         string            `json:"UsernsMode"`
			IpcMode            string            `json:"IpcMode"`
			PidMode            string            `json:"PidMode"`
			UTSMode            string            `json:"UTSMode"`
			PublishAll         bool              `json:"PublishAllPorts"`
			Memory             int64             `json:"Memory"`
			NanoCPUs           int64             `json:"NanoCpus"`
			CPUPeriod          int64             `json:"CpuPeriod"`
			CPUQuota           int64             `json:"CpuQuota"`
			CPUCount           int64             `json:"CpuCount"`
			CPUPercent         int64             `json:"CpuPercent"`
			CPUShares          int64             `json:"CpuShares"`
			CPUSetCPUs         string            `json:"CpusetCpus"`
			CPUSetMems         string            `json:"CpusetMems"`
			BlkioWeight        uint16            `json:"BlkioWeight"`
			MemoryReservation  int64             `json:"MemoryReservation"`
			MemorySwap         int64             `json:"MemorySwap"`
			MemorySwappiness   *int64            `json:"MemorySwappiness"`
			OomScoreAdj        int               `json:"OomScoreAdj"`
			CPURealtimePeriod  int64             `json:"CpuRealtimePeriod"`
			CPURealtimeRuntime int64             `json:"CpuRealtimeRuntime"`
			IOMaximumIOps      uint64            `json:"IOMaximumIOps"`
			IOMaximumBandwidth uint64            `json:"IOMaximumBandwidth"`
			Init               *bool             `json:"Init"`
			PidsLimit          *int64            `json:"PidsLimit"`
			OomKillDisable     *bool             `json:"OomKillDisable"`
		} `json:"HostConfig"`
	}
	if !decodeDockerJSON(w, r, &request) {
		return
	}
	if request.HostConfig.Privileged {
		writeDockerUnsupported(w, "privileged containers")
		return
	}
	unsupported := make([]string, 0)
	if request.NetworkDisabled {
		unsupported = append(unsupported, "NetworkDisabled")
	}
	if request.HostConfig.ReadonlyRoot {
		unsupported = append(unsupported, "ReadonlyRootfs")
	}
	if len(request.HostConfig.Devices) > 0 {
		unsupported = append(unsupported, "Devices")
	}
	if len(request.HostConfig.DeviceRequests) > 0 {
		unsupported = append(unsupported, "DeviceRequests")
	}
	if len(request.HostConfig.DeviceCgroupRules) > 0 {
		unsupported = append(unsupported, "DeviceCgroupRules")
	}
	if len(request.HostConfig.Mounts) > 0 {
		unsupported = append(unsupported, "Mounts")
	}
	if len(request.HostConfig.CapAdd) > 0 {
		unsupported = append(unsupported, "CapAdd")
	}
	if len(request.HostConfig.CapDrop) > 0 {
		unsupported = append(unsupported, "CapDrop")
	}
	if len(request.HostConfig.SecurityOpt) > 0 {
		unsupported = append(unsupported, "SecurityOpt")
	}
	if len(request.HostConfig.ExtraHosts) > 0 {
		unsupported = append(unsupported, "ExtraHosts")
	}
	if len(request.HostConfig.DNS) > 0 {
		unsupported = append(unsupported, "Dns")
	}
	if len(request.HostConfig.DNSOptions) > 0 || len(request.HostConfig.DNSSearch) > 0 {
		unsupported = append(unsupported, "DNS options")
	}
	if len(request.HostConfig.GroupAdd) > 0 {
		unsupported = append(unsupported, "GroupAdd")
	}
	if len(request.HostConfig.Ulimits) > 0 {
		unsupported = append(unsupported, "Ulimits")
	}
	if len(request.HostConfig.MaskedPaths) > 0 {
		unsupported = append(unsupported, "MaskedPaths")
	}
	if len(request.HostConfig.ReadonlyPaths) > 0 {
		unsupported = append(unsupported, "ReadonlyPaths")
	}
	if len(request.HostConfig.Sysctls) > 0 {
		unsupported = append(unsupported, "Sysctls")
	}
	if len(request.HostConfig.Tmpfs) > 0 {
		unsupported = append(unsupported, "Tmpfs")
	}
	if len(request.HostConfig.StorageOpt) > 0 {
		unsupported = append(unsupported, "StorageOpt")
	}
	if request.HostConfig.PublishAll {
		unsupported = append(unsupported, "PublishAllPorts")
	}
	if request.HostConfig.Memory != 0 {
		unsupported = append(unsupported, "Memory")
	}
	if request.HostConfig.NanoCPUs != 0 {
		unsupported = append(unsupported, "NanoCpus")
	}
	if request.HostConfig.CPUPeriod != 0 || request.HostConfig.CPUQuota != 0 || request.HostConfig.CPUCount != 0 ||
		request.HostConfig.CPUPercent != 0 || request.HostConfig.CPUShares != 0 ||
		request.HostConfig.CPUSetCPUs != "" || request.HostConfig.CPUSetMems != "" {
		unsupported = append(unsupported, "CPU constraints")
	}
	if request.HostConfig.BlkioWeight != 0 {
		unsupported = append(unsupported, "BlkioWeight")
	}
	if request.HostConfig.MemoryReservation != 0 || request.HostConfig.MemorySwap != 0 ||
		(request.HostConfig.MemorySwappiness != nil &&
			*request.HostConfig.MemorySwappiness != 0 &&
			*request.HostConfig.MemorySwappiness != -1) {
		unsupported = append(unsupported, "memory constraints")
	}
	if request.HostConfig.OomScoreAdj != 0 {
		unsupported = append(unsupported, "OomScoreAdj")
	}
	if request.HostConfig.CPURealtimePeriod != 0 || request.HostConfig.CPURealtimeRuntime != 0 {
		unsupported = append(unsupported, "CPU realtime constraints")
	}
	if request.HostConfig.IOMaximumIOps != 0 || request.HostConfig.IOMaximumBandwidth != 0 {
		unsupported = append(unsupported, "I/O constraints")
	}
	if request.HostConfig.Init != nil && *request.HostConfig.Init {
		unsupported = append(unsupported, "Init")
	}
	if request.HostConfig.PidsLimit != nil && *request.HostConfig.PidsLimit != 0 && *request.HostConfig.PidsLimit != -1 {
		unsupported = append(unsupported, "PidsLimit")
	}
	if request.HostConfig.OomKillDisable != nil && *request.HostConfig.OomKillDisable {
		unsupported = append(unsupported, "OomKillDisable")
	}
	if request.HostConfig.Runtime != "" && request.HostConfig.Runtime != "runc" && request.HostConfig.Runtime != "io.containerd.runc.v2" {
		unsupported = append(unsupported, "Runtime")
	}
	if request.HostConfig.CgroupnsMode != "" && request.HostConfig.CgroupnsMode != "private" {
		unsupported = append(unsupported, "CgroupnsMode")
	}
	if request.HostConfig.UsernsMode != "" {
		unsupported = append(unsupported, "UsernsMode")
	}
	if request.HostConfig.IpcMode != "" && request.HostConfig.IpcMode != "private" {
		unsupported = append(unsupported, "IpcMode")
	}
	if request.HostConfig.PidMode != "" {
		unsupported = append(unsupported, "PidMode")
	}
	if request.HostConfig.UTSMode != "" {
		unsupported = append(unsupported, "UTSMode")
	}
	if request.HostConfig.Isolation != "" && request.HostConfig.Isolation != "default" {
		unsupported = append(unsupported, "Isolation")
	}
	if request.HostConfig.CgroupParent != "" || request.HostConfig.Cgroup != "" {
		unsupported = append(unsupported, "custom cgroup placement")
	}
	if len(unsupported) > 0 {
		writeDockerUnsupported(w, "container options: "+strings.Join(unsupported, ", "))
		return
	}
	publish := make([]string, 0)
	for containerPort, bindings := range request.HostConfig.PortBindings {
		for _, binding := range bindings {
			target := binding.HostPort
			if binding.HostIP != "" {
				target = binding.HostIP + ":" + target
			}
			if target != "" {
				target += ":"
			}
			publish = append(publish, target+containerPort)
		}
	}
	restartPolicy := request.HostConfig.RestartPolicy.Name
	if request.HostConfig.RestartPolicy.MaximumRetryCount != 0 {
		if restartPolicy != "on-failure" {
			writeDockerUnsupported(w, "restart retry count without on-failure policy")
			return
		}
		restartPolicy += ":" + strconv.Itoa(request.HostConfig.RestartPolicy.MaximumRetryCount)
	}
	id, err := a.manager.CreateContainer(r.Context(), CreateContainerRequest{
		Name:        r.URL.Query().Get("name"),
		Image:       request.Image,
		Platform:    r.URL.Query().Get("platform"),
		Command:     request.Cmd,
		Entrypoint:  request.Entrypoint,
		Environment: request.Env,
		Labels:      request.Labels,
		WorkingDir:  request.WorkingDir,
		User:        request.User,
		Hostname:    request.Hostname,
		Network:     request.HostConfig.NetworkMode,
		Volumes:     request.HostConfig.Binds,
		Publish:     publish,
		Restart:     restartPolicy,
		TTY:         request.Tty,
		Interactive: request.OpenStdin || request.AttachStdin,
		Remove:      request.HostConfig.AutoRemove,
	})
	if err != nil {
		writeDockerError(w, err)
		return
	}
	writeDockerJSON(w, http.StatusCreated, map[string]any{"Id": id, "Warnings": []string{}})
}

func (a *API) inspectContainer(w http.ResponseWriter, r *http.Request) {
	value, err := a.manager.InspectContainer(r.Context(), r.PathValue("id"))
	writeDockerRaw(w, value, err)
}

func (a *API) containerAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if timeout := r.URL.Query().Get("t"); timeout != "" {
			writeDockerUnsupported(w, action+" timeout")
			return
		}
		if err := a.manager.ContainerAction(r.Context(), r.PathValue("id"), action); err != nil {
			writeDockerError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (a *API) renameContainer(w http.ResponseWriter, r *http.Request) {
	if err := a.manager.RenameContainer(r.Context(), r.PathValue("id"), r.URL.Query().Get("name")); err != nil {
		writeDockerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) waitContainer(w http.ResponseWriter, r *http.Request) {
	code, err := a.manager.WaitContainer(r.Context(), r.PathValue("id"), r.URL.Query().Get("condition"))
	if err != nil {
		writeDockerError(w, err)
		return
	}
	writeDockerJSON(w, http.StatusOK, map[string]any{
		"StatusCode": code,
		"Error":      nil,
	})
}

func (a *API) containerLogs(w http.ResponseWriter, r *http.Request) {
	if dockerBool(r, "follow") {
		writeDockerUnsupported(w, "following container logs")
		return
	}
	stdout := dockerBool(r, "stdout")
	stderr := dockerBool(r, "stderr")
	if !stdout && !stderr {
		stdout, stderr = true, true
	}
	tail := r.URL.Query().Get("tail")
	if tail == "" {
		tail = "all"
	}
	if tail != "all" {
		if count, err := strconv.Atoi(tail); err != nil || count < 0 {
			writeDockerJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid log tail value"})
			return
		}
	}
	tty, err := a.manager.ContainerTTY(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDockerError(w, err)
		return
	}
	wrote := false
	err = a.manager.StreamDockerContainerLogs(r.Context(), r.PathValue("id"), LogOptions{
		Stdout:     stdout,
		Stderr:     stderr,
		Timestamps: dockerBool(r, "timestamps"),
		Tail:       tail,
		Since:      r.URL.Query().Get("since"),
		Until:      r.URL.Query().Get("until"),
	}, func(chunk runtimes.OutputChunk) error {
		if !wrote {
			w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
			w.WriteHeader(http.StatusOK)
			wrote = true
		}
		if tty {
			_, writeErr := w.Write(chunk.Data)
			return writeErr
		}
		stream := byte(1)
		if chunk.Stream == "stderr" {
			stream = 2
		}
		_, writeErr := w.Write(dockerStreamFrame(stream, chunk.Data))
		return writeErr
	})
	if err != nil {
		if wrote {
			return
		}
		writeDockerError(w, err)
		return
	}
	if !wrote {
		w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
		w.WriteHeader(http.StatusOK)
	}
}

func (a *API) deleteContainer(w http.ResponseWriter, r *http.Request) {
	if dockerBool(r, "v") || dockerBool(r, "link") {
		writeDockerUnsupported(w, "container removal with volume or link cleanup")
		return
	}
	action := "remove"
	if dockerBool(r, "force") {
		action = "remove-force"
	}
	if err := a.manager.ContainerAction(r.Context(), r.PathValue("id"), action); err != nil {
		writeDockerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) images(w http.ResponseWriter, r *http.Request) {
	for _, parameter := range []string{"filters", "shared-size"} {
		if r.URL.Query().Get(parameter) != "" {
			writeDockerUnsupported(w, "image list query parameter "+parameter)
			return
		}
	}
	if dockerBool(r, "all") {
		writeDockerUnsupported(w, "listing intermediate images")
		return
	}
	images, err := a.manager.Images(r.Context())
	if err != nil {
		writeDockerError(w, err)
		return
	}
	response := make([]map[string]any, 0, len(images))
	for _, image := range images {
		tag := image.Repository
		if image.Tag != "" && image.Tag != "<none>" {
			tag += ":" + image.Tag
		}
		tags := []string{}
		if tag != "" && tag != "<none>" {
			tags = append(tags, tag)
		}
		digests := []string{}
		if image.Digest != "" && image.Digest != "<none>" {
			digests = append(digests, image.Repository+"@"+image.Digest)
		}
		response = append(response, map[string]any{
			"Id":          image.ID,
			"ParentId":    "",
			"RepoTags":    tags,
			"RepoDigests": digests,
			"Created":     parseDockerTime(image.CreatedAt),
			"Size":        int64(0),
			"SharedSize":  int64(-1),
			"VirtualSize": int64(0),
			"Labels":      image.Labels,
			"Containers":  int64(-1),
		})
	}
	writeDockerJSON(w, http.StatusOK, response)
}

func (a *API) inspectImage(w http.ResponseWriter, r *http.Request) {
	value, err := a.manager.InspectImage(r.Context(), r.PathValue("id"))
	writeDockerRaw(w, value, err)
}

func (a *API) pullImage(w http.ResponseWriter, r *http.Request) {
	reference := r.URL.Query().Get("fromImage")
	if tag := r.URL.Query().Get("tag"); tag != "" && !strings.Contains(reference, "@") {
		reference += ":" + tag
	}
	if err := a.manager.PullImage(r.Context(), reference, r.URL.Query().Get("platform")); err != nil {
		writeDockerError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "Image is up to date for " + reference})
}

func (a *API) deleteImage(w http.ResponseWriter, r *http.Request) {
	if dockerBool(r, "noprune") {
		writeDockerUnsupported(w, "image removal without parent pruning")
		return
	}
	id := r.PathValue("id")
	if err := a.manager.RemoveImage(r.Context(), id, dockerBool(r, "force")); err != nil {
		writeDockerError(w, err)
		return
	}
	writeDockerJSON(w, http.StatusOK, []map[string]string{{"Deleted": id}})
}

func (a *API) networks(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("filters") != "" {
		writeDockerUnsupported(w, "network filters")
		return
	}
	networks, err := a.manager.Networks(r.Context())
	if err != nil {
		writeDockerError(w, err)
		return
	}
	response := make([]map[string]any, 0, len(networks))
	for _, network := range networks {
		response = append(response, dockerNetwork(network))
	}
	writeDockerJSON(w, http.StatusOK, response)
}

func (a *API) createNetwork(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name       string            `json:"Name"`
		Driver     string            `json:"Driver"`
		Internal   bool              `json:"Internal"`
		EnableIPv6 bool              `json:"EnableIPv6"`
		Labels     map[string]string `json:"Labels"`
		IPAM       struct {
			Config []struct {
				Subnet  string `json:"Subnet"`
				Gateway string `json:"Gateway"`
			} `json:"Config"`
		} `json:"IPAM"`
	}
	if !decodeDockerJSON(w, r, &request) {
		return
	}
	if request.EnableIPv6 {
		writeDockerUnsupported(w, "IPv6 network creation")
		return
	}
	subnet, gateway := "", ""
	if len(request.IPAM.Config) > 0 {
		subnet = request.IPAM.Config[0].Subnet
		gateway = request.IPAM.Config[0].Gateway
	}
	id, err := a.manager.CreateNetwork(r.Context(), CreateNetworkRequest{
		Name: request.Name, Driver: request.Driver, Subnet: subnet, Gateway: gateway,
		Internal: request.Internal, Labels: request.Labels,
	})
	if err != nil {
		writeDockerError(w, err)
		return
	}
	writeDockerJSON(w, http.StatusCreated, map[string]any{"Id": id, "Warning": ""})
}

func (a *API) inspectNetwork(w http.ResponseWriter, r *http.Request) {
	value, err := a.manager.InspectNetwork(r.Context(), r.PathValue("id"))
	writeDockerRaw(w, value, err)
}

func (a *API) deleteNetwork(w http.ResponseWriter, r *http.Request) {
	if err := a.manager.RemoveNetwork(r.Context(), r.PathValue("id")); err != nil {
		writeDockerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) volumes(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("filters") != "" {
		writeDockerUnsupported(w, "volume filters")
		return
	}
	volumes, err := a.manager.Volumes(r.Context())
	if err != nil {
		writeDockerError(w, err)
		return
	}
	response := make([]map[string]any, 0, len(volumes))
	for _, volume := range volumes {
		response = append(response, dockerVolume(volume))
	}
	writeDockerJSON(w, http.StatusOK, map[string]any{"Volumes": response, "Warnings": []string{}})
}

func (a *API) createVolume(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name       string            `json:"Name"`
		Driver     string            `json:"Driver"`
		DriverOpts map[string]string `json:"DriverOpts"`
		Labels     map[string]string `json:"Labels"`
	}
	if !decodeDockerJSON(w, r, &request) {
		return
	}
	if len(request.DriverOpts) > 0 {
		writeDockerUnsupported(w, "volume driver options")
		return
	}
	volume, err := a.manager.CreateVolume(r.Context(), request.Name, request.Driver, request.Labels)
	if err != nil {
		writeDockerError(w, err)
		return
	}
	writeDockerJSON(w, http.StatusCreated, dockerVolume(volume))
}

func (a *API) inspectVolume(w http.ResponseWriter, r *http.Request) {
	value, err := a.manager.InspectVolume(r.Context(), r.PathValue("name"))
	writeDockerRaw(w, value, err)
}

func (a *API) deleteVolume(w http.ResponseWriter, r *http.Request) {
	if err := a.manager.RemoveVolume(r.Context(), r.PathValue("name"), dockerBool(r, "force")); err != nil {
		writeDockerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) unsupported(w http.ResponseWriter, r *http.Request) {
	writeDockerJSON(w, http.StatusNotImplemented, map[string]string{
		"message": fmt.Sprintf("Porto Docker API does not support %s %s", r.Method, r.URL.Path),
	})
}

func stripAPIVersion(path string) string {
	if !strings.HasPrefix(path, "/v") {
		return path
	}
	remainder := path[2:]
	slash := strings.IndexByte(remainder, '/')
	if slash < 0 {
		return path
	}
	version := remainder[:slash]
	for _, character := range version {
		if (character < '0' || character > '9') && character != '.' {
			return path
		}
	}
	return remainder[slash:]
}

func decodeDockerJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*1024*1024))
	if err := decoder.Decode(value); err != nil {
		writeDockerJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid Docker request: " + err.Error()})
		return false
	}
	return true
}

func writeDockerRaw(w http.ResponseWriter, value json.RawMessage, err error) {
	if err != nil {
		writeDockerError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(value)
}

func writeDockerJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeDockerError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	message := strings.ToLower(err.Error())
	switch {
	case errors.Is(err, ErrUnsupported), strings.Contains(message, "unsupported"):
		status = http.StatusNotImplemented
	case errors.Is(err, ErrUnavailable), strings.Contains(message, "unavailable"):
		status = http.StatusServiceUnavailable
	case strings.Contains(message, "not found"), strings.Contains(message, "no such"):
		status = http.StatusNotFound
	case strings.Contains(message, "already exists"), strings.Contains(message, "conflict"):
		status = http.StatusConflict
	case strings.Contains(message, "required"), strings.Contains(message, "invalid"):
		status = http.StatusBadRequest
	}
	writeDockerJSON(w, status, map[string]string{"message": err.Error()})
}

func writeDockerUnsupported(w http.ResponseWriter, operation string) {
	writeDockerError(w, fmt.Errorf("%w: %s", ErrUnsupported, operation))
}

func dockerBool(r *http.Request, name string) bool {
	value := strings.ToLower(strings.TrimSpace(r.URL.Query().Get(name)))
	return value == "1" || value == "true" || value == "yes"
}

func parseDockerTime(value string) int64 {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05 -0700 MST"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.Unix()
		}
	}
	return 0
}

func dockerNetwork(network Network) map[string]any {
	return map[string]any{
		"Name":       network.Name,
		"Id":         network.ID,
		"Created":    network.Created,
		"Scope":      firstNonEmpty(network.Scope, "local"),
		"Driver":     firstNonEmpty(network.Driver, "bridge"),
		"EnableIPv6": strings.EqualFold(network.IPv6, "true"),
		"IPAM":       map[string]any{"Driver": "default", "Options": nil, "Config": []any{}},
		"Internal":   strings.EqualFold(network.Internal, "true"),
		"Attachable": false,
		"Ingress":    false,
		"ConfigFrom": map[string]string{"Network": ""},
		"ConfigOnly": false,
		"Containers": map[string]any{},
		"Options":    map[string]string{},
		"Labels":     network.Labels,
	}
}

func dockerVolume(volume Volume) map[string]any {
	return map[string]any{
		"CreatedAt":  volume.CreatedAt,
		"Driver":     firstNonEmpty(volume.Driver, "local"),
		"Labels":     volume.Labels,
		"Mountpoint": volume.Mountpoint,
		"Name":       volume.Name,
		"Options":    map[string]string{},
		"Scope":      firstNonEmpty(volume.Scope, "local"),
	}
}

func dockerStreamFrame(stream byte, output []byte) []byte {
	if len(output) == 0 {
		return nil
	}
	frame := make([]byte, 8+len(output))
	frame[0] = stream
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(output)))
	copy(frame[8:], output)
	return frame
}
