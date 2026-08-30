package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/mbianchidev/porto/internal/config"
	portodocker "github.com/mbianchidev/porto/internal/docker"
	"github.com/mbianchidev/porto/internal/kubernetes"
	"github.com/mbianchidev/porto/internal/store"
	"github.com/mbianchidev/porto/internal/vm"
)

func runtimeCmd(st *store.Store, args []string) error {
	if len(args) == 0 || args[0] == "status" {
		if daemonUp() {
			return api("GET", "/api/runtime", nil, os.Stdout)
		}
		settings, err := st.Settings(context.Background())
		if err != nil {
			return err
		}
		return writeOutput(map[string]bool{
			"docker":     settings.DockerEnabled,
			"kubernetes": settings.KubernetesEnabled,
			"vms":        settings.VMsEnabled,
		})
	}
	if len(args) != 2 || (args[0] != "enable" && args[0] != "disable") {
		return errors.New("usage: porto runtime status|enable|disable <docker|kubernetes|vms>")
	}
	feature := args[1]
	if feature != "docker" && feature != "kubernetes" && feature != "vms" {
		return fmt.Errorf("unknown runtime feature %q", feature)
	}
	if daemonUp() {
		return api("POST", "/api/runtime/features/"+feature+"/"+args[0], nil, os.Stdout)
	}
	settings, err := st.Settings(context.Background())
	if err != nil {
		return err
	}
	enabled := args[0] == "enable"
	switch feature {
	case "docker":
		settings.DockerEnabled = enabled
	case "kubernetes":
		settings.KubernetesEnabled = enabled
	case "vms":
		settings.VMsEnabled = enabled
	}
	if err := st.SetSettings(context.Background(), settings); err != nil {
		return err
	}
	return writeOutput(map[string]bool{feature: enabled})
}

func dockerCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: porto docker status|containers|images|builds|networks|volumes|context-install|activate|deactivate")
	}
	switch args[0] {
	case "status":
		if daemonUp() {
			return api("GET", "/api/docker/status", nil, os.Stdout)
		}
		socketPath, _ := config.DockerSocketPath()
		return writeOutput(portodocker.New(nil).Status(context.Background(), socketPath))
	case "containers", "images", "builds", "networks", "volumes":
		return runtimeGET("/api/docker/" + args[0])
	case "context-install":
		if len(args) != 1 {
			return errors.New("usage: porto docker context-install")
		}
		if runtime.GOOS == "windows" {
			return errors.New("Porto Docker named-pipe proxy is not available in this build")
		}
		socketPath, err := config.DockerSocketPath()
		if err != nil {
			return err
		}
		if err := portodocker.New(nil).InstallContext(context.Background(), socketPath); err != nil {
			return err
		}
		return writeOutput(map[string]string{"context": "porto", "endpoint": "unix://" + socketPath})
	case "activate":
		if runtime.GOOS == "windows" {
			return errors.New("canonical Docker named-pipe activation is not available in this build")
		}
		fs := flag.NewFlagSet("docker activate", flag.ContinueOnError)
		replace := fs.Bool("replace", false, "replace an existing Docker socket symlink and remember it for restoration")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("usage: porto docker activate [--replace]")
		}
		if !daemonUp() {
			return errors.New("daemon is not running; start it before activating the Docker endpoint")
		}
		socketPath, statePath, err := dockerEndpointPaths()
		if err != nil {
			return err
		}
		state, err := portodocker.ActivateEndpoint(config.CanonicalDockerSocketPath(), socketPath, statePath, *replace)
		if err != nil {
			return dockerPrivilegeHint(err)
		}
		return writeOutput(state)
	case "deactivate":
		if runtime.GOOS == "windows" {
			return errors.New("canonical Docker named-pipe deactivation is not available in this build")
		}
		if len(args) != 1 {
			return errors.New("usage: porto docker deactivate")
		}
		_, statePath, err := dockerEndpointPaths()
		if err != nil {
			return err
		}
		if err := portodocker.DeactivateEndpoint(statePath); err != nil {
			return dockerPrivilegeHint(err)
		}
		return writeOutput(map[string]string{"status": "deactivated"})
	case "container":
		if len(args) != 3 {
			return errors.New("usage: porto docker container <start|stop|restart|pause|unpause|remove|remove-force> <id>")
		}
		return runtimePOST("/api/docker/containers/"+url.PathEscape(args[2])+"/"+url.PathEscape(args[1]), nil)
	case "pull":
		if len(args) != 2 {
			return errors.New("usage: porto docker pull <image>")
		}
		return runtimePOST("/api/docker/images/pull", map[string]string{"reference": args[1]})
	case "build":
		return dockerBuildCmd(args[1:])
	default:
		return fmt.Errorf("unsupported Docker command %q", args[0])
	}
}

func dockerBuildCmd(args []string) error {
	fs := flag.NewFlagSet("docker build", flag.ContinueOnError)
	tag := fs.String("tag", "", "image tag")
	dockerfile := fs.String("file", "", "Dockerfile path")
	target := fs.String("target", "", "build target")
	platform := fs.String("platform", "", "target platform")
	noCache := fs.Bool("no-cache", false, "disable build cache")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: porto docker build <context> [--tag name] [--file Dockerfile] [--target stage] [--platform platform] [--no-cache]")
	}
	return runtimePOST("/api/docker/builds", portodocker.BuildRequest{
		Context:    fs.Arg(0),
		Dockerfile: *dockerfile,
		Tag:        *tag,
		Target:     *target,
		Platform:   *platform,
		NoCache:    *noCache,
	})
}

func kubernetesCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: porto kubernetes status|contexts|pods|services|nodes|logs|exec|files|cluster")
	}
	switch args[0] {
	case "status", "contexts", "nodes", "clusters":
		return runtimeGET("/api/kubernetes/" + args[0])
	case "kubeconfig":
		if len(args) != 2 {
			return errors.New("usage: porto kubernetes kubeconfig <cluster>")
		}
		path, err := clusterKubeconfigPath(args[1])
		if err != nil {
			return err
		}
		return writeOutput(map[string]string{"cluster": args[1], "context": "porto-" + args[1], "path": path})
	case "context-install":
		if len(args) != 2 {
			return errors.New("usage: porto kubernetes context-install <cluster>")
		}
		return installClusterContext(args[1])
	case "pods", "services":
		fs := flag.NewFlagSet("kubernetes "+args[0], flag.ContinueOnError)
		contextName := fs.String("context", "", "Kubernetes context")
		namespace := fs.String("namespace", "all", "namespace or all")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		query := url.Values{"namespace": {*namespace}}
		if *contextName != "" {
			query.Set("context", *contextName)
		}
		return runtimeGET("/api/kubernetes/" + args[0] + "?" + query.Encode())
	case "logs":
		return kubernetesLogsCmd(args[1:])
	case "exec":
		return kubernetesExecCmd(args[1:])
	case "files":
		return kubernetesFilesCmd(args[1:])
	case "cluster":
		return kubernetesClusterCmd(args[1:])
	default:
		return fmt.Errorf("unsupported Kubernetes command %q", args[0])
	}
}

func kubernetesLogsCmd(args []string) error {
	fs := flag.NewFlagSet("kubernetes logs", flag.ContinueOnError)
	contextName := fs.String("context", "", "Kubernetes context")
	container := fs.String("container", "", "container name")
	previous := fs.Bool("previous", false, "show the previous container instance")
	tail := fs.Int("tail", 500, "maximum lines")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("usage: porto kubernetes logs <namespace> <pod> [--context name] [--container name] [--previous] [--tail 500]")
	}
	query := url.Values{"tail": {strconv.Itoa(*tail)}}
	if *contextName != "" {
		query.Set("context", *contextName)
	}
	if *container != "" {
		query.Set("container", *container)
	}
	if *previous {
		query.Set("previous", "true")
	}
	path := "/api/kubernetes/pods/" + url.PathEscape(fs.Arg(0)) + "/" + url.PathEscape(fs.Arg(1)) + "/logs?" + query.Encode()
	return runtimeGET(path)
}

func kubernetesExecCmd(args []string) error {
	separator := -1
	for index, arg := range args {
		if arg == "--" {
			separator = index
			break
		}
	}
	flagArgs := args
	command := []string(nil)
	if separator >= 0 {
		flagArgs = args[:separator]
		command = args[separator+1:]
	}
	fs := flag.NewFlagSet("kubernetes exec", flag.ContinueOnError)
	contextName := fs.String("context", "", "Kubernetes context")
	container := fs.String("container", "", "container name")
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if fs.NArg() != 2 || len(command) == 0 {
		return errors.New("usage: porto kubernetes exec <namespace> <pod> [--context name] [--container name] -- <command...>")
	}
	query := url.Values{}
	if *contextName != "" {
		query.Set("context", *contextName)
	}
	path := "/api/kubernetes/pods/" + url.PathEscape(fs.Arg(0)) + "/" + url.PathEscape(fs.Arg(1)) + "/exec"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	return runtimePOST(path, map[string]any{"container": *container, "command": command})
}

func kubernetesFilesCmd(args []string) error {
	fs := flag.NewFlagSet("kubernetes files", flag.ContinueOnError)
	contextName := fs.String("context", "", "Kubernetes context")
	container := fs.String("container", "", "container name")
	remotePath := fs.String("path", "/", "container path")
	read := fs.Bool("read", false, "read a file instead of listing a directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("usage: porto kubernetes files <namespace> <pod> [--context name] [--container name] [--path /] [--read]")
	}
	query := url.Values{"path": {*remotePath}}
	if *contextName != "" {
		query.Set("context", *contextName)
	}
	if *container != "" {
		query.Set("container", *container)
	}
	resource := "files"
	if *read {
		resource = "file"
	}
	path := "/api/kubernetes/pods/" + url.PathEscape(fs.Arg(0)) + "/" + url.PathEscape(fs.Arg(1)) + "/" + resource + "?" + query.Encode()
	return runtimeGET(path)
}

func kubernetesClusterCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: porto kubernetes cluster create|start|stop|delete")
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("kubernetes cluster create", flag.ContinueOnError)
		version := fs.String("version", "", "pinned k3s version")
		cpus := fs.Int("cpus", 2, "control-plane CPUs")
		memory := fs.Int("memory", 2048, "control-plane memory MiB")
		disk := fs.Int("disk", 20, "control-plane disk GiB")
		workers := fs.Int("workers", 1, "worker count")
		workerCPUs := fs.Int("worker-cpus", 2, "CPUs per worker")
		workerMemory := fs.Int("worker-memory", 2048, "memory MiB per worker")
		workerDisk := fs.Int("worker-disk", 20, "disk GiB per worker")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New("usage: porto kubernetes cluster create <name> [--version v...] [--workers 1]")
		}
		return runtimePOST("/api/kubernetes/clusters", kubernetes.ClusterRequest{
			Name:    fs.Arg(0),
			Version: *version,
			ControlPlane: kubernetes.MachineSpec{
				CPUs: *cpus, MemoryMiB: *memory, DiskGiB: *disk,
			},
			NodeGroups: []kubernetes.NodeGroupSpec{{
				Name: "workers", Count: *workers,
				Machine: kubernetes.MachineSpec{CPUs: *workerCPUs, MemoryMiB: *workerMemory, DiskGiB: *workerDisk},
			}},
		})
	case "start", "stop":
		if len(args) != 2 {
			return fmt.Errorf("usage: porto kubernetes cluster %s <name>", args[0])
		}
		return runtimePOST("/api/kubernetes/clusters/"+url.PathEscape(args[1])+"/"+args[0], nil)
	case "scale":
		fs := flag.NewFlagSet("kubernetes cluster scale", flag.ContinueOnError)
		count := fs.Int("nodes", -1, "desired node count")
		cpus := fs.Int("cpus", 2, "CPUs per node")
		memory := fs.Int("memory", 2048, "memory MiB per node")
		disk := fs.Int("disk", 20, "disk GiB per node")
		version := fs.String("version", "", "pinned k3s version for new nodes")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 2 || *count < 0 {
			return errors.New("usage: porto kubernetes cluster scale <cluster> <group> --nodes <count>")
		}
		request := map[string]any{
			"version": *version,
			"count":   *count,
			"machine": kubernetes.MachineSpec{CPUs: *cpus, MemoryMiB: *memory, DiskGiB: *disk},
		}
		return runtimePOST(
			"/api/kubernetes/clusters/"+url.PathEscape(fs.Arg(0))+"/node-groups/"+url.PathEscape(fs.Arg(1)),
			request,
		)
	case "delete":
		if len(args) != 2 {
			return errors.New("usage: porto kubernetes cluster delete <name>")
		}
		return runtimeDELETE("/api/kubernetes/clusters/" + url.PathEscape(args[1]) + "?confirm=true")
	default:
		return fmt.Errorf("unsupported Kubernetes cluster command %q", args[0])
	}
}

func vmCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: porto vm status|images|list|create|start|stop|delete|exec|snapshot|restore")
	}
	switch args[0] {
	case "status":
		return runtimeGET("/api/vms/status")
	case "images":
		return runtimeGET("/api/vms/images")
	case "list":
		return runtimeGET("/api/vms/instances")
	case "create":
		fs := flag.NewFlagSet("vm create", flag.ContinueOnError)
		image := fs.String("image", "ubuntu-24.04", "VM image ID")
		cpus := fs.Int("cpus", 2, "virtual CPUs")
		memory := fs.Int("memory", 2048, "memory MiB")
		disk := fs.Int("disk", 20, "disk GiB")
		architecture := fs.String("arch", "", "aarch64 or x86_64")
		provision := fs.String("provision", "", "post-start shell command")
		stopped := fs.Bool("stopped", false, "create without starting")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New("usage: porto vm create <name> [--image ubuntu-24.04] [--cpus 2] [--memory 2048] [--disk 20]")
		}
		return runtimePOST("/api/vms/instances", vm.CreateRequest{
			Name: fs.Arg(0), Image: *image, CPUs: *cpus, MemoryMiB: *memory,
			DiskGiB: *disk, Architecture: *architecture, Provision: *provision, Start: !*stopped,
		})
	case "start", "stop":
		if len(args) != 2 {
			return fmt.Errorf("usage: porto vm %s <name>", args[0])
		}
		return runtimePOST("/api/vms/instances/"+url.PathEscape(args[1])+"/"+args[0], nil)
	case "delete":
		if len(args) != 2 {
			return errors.New("usage: porto vm delete <name>")
		}
		return runtimeDELETE("/api/vms/instances/" + url.PathEscape(args[1]) + "?confirm=true&force=true")
	case "exec":
		if len(args) < 3 {
			return errors.New("usage: porto vm exec <name> <command...>")
		}
		return runtimePOST("/api/vms/instances/"+url.PathEscape(args[1])+"/exec", map[string]any{"command": args[2:]})
	case "snapshot", "restore":
		if len(args) != 3 {
			return fmt.Errorf("usage: porto vm %s <name> <snapshot>", args[0])
		}
		return runtimePOST("/api/vms/instances/"+url.PathEscape(args[1])+"/"+args[0], map[string]string{"name": args[2]})
	default:
		return fmt.Errorf("unsupported VM command %q", args[0])
	}
}

func runtimeResourceAlias(command string, args []string) error {
	resource := strings.TrimSuffix(command, "s")
	switch resource {
	case "container", "image", "build", "network", "volume":
	default:
		return fmt.Errorf("unsupported resource %q", command)
	}
	if len(args) == 0 || args[0] == "list" {
		return runtimeGET("/api/docker/" + resource + "s")
	}
	return fmt.Errorf("usage: porto %s list", resource)
}

func runtimeGET(path string) error {
	if !daemonUp() {
		return errors.New("daemon is not running; start it with 'porto daemon start'")
	}
	return api("GET", path, nil, os.Stdout)
}

func runtimePOST(path string, body any) error {
	if !daemonUp() {
		return errors.New("daemon is not running; start it with 'porto daemon start'")
	}
	return api("POST", path, body, os.Stdout)
}

func runtimeDELETE(path string) error {
	if !daemonUp() {
		return errors.New("daemon is not running; start it with 'porto daemon start'")
	}
	return api("DELETE", path, nil, os.Stdout)
}

func dockerEndpointPaths() (string, string, error) {
	socketPath, err := config.DockerSocketPath()
	if err != nil {
		return "", "", err
	}
	statePath, err := config.DockerEndpointStatePath()
	if err != nil {
		return "", "", err
	}
	return socketPath, statePath, nil
}

func dockerPrivilegeHint(err error) error {
	message := strings.ToLower(err.Error())
	if !strings.Contains(message, "permission denied") && !strings.Contains(message, "operation not permitted") {
		return err
	}
	executable, executableErr := os.Executable()
	if executableErr != nil {
		executable = "porto"
	}
	home, _ := config.Dir()
	return fmt.Errorf("%w; retry with administrator privileges while preserving Porto state: sudo PORTO_HOME=%s %s docker %s",
		err,
		shellDisplay(home),
		shellDisplay(filepath.Clean(executable)),
		activationVerb(err),
	)
}

func activationVerb(err error) string {
	if strings.Contains(strings.ToLower(err.Error()), "remove") {
		return "deactivate"
	}
	return "activate"
}

func shellDisplay(value string) string {
	if !strings.ContainsAny(value, " \t'\"") {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func clusterKubeconfigPath(cluster string) (string, error) {
	if cluster == "" || strings.ContainsAny(cluster, `/\\`+"\x00\r\n") {
		return "", errors.New("invalid Kubernetes cluster name")
	}
	dir, err := config.KubernetesConfigDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, cluster+".yaml")
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		if err == nil {
			err = errors.New("path is a directory")
		}
		return "", fmt.Errorf("Porto kubeconfig for %s is unavailable: %w", cluster, err)
	}
	return path, nil
}

func installClusterContext(cluster string) error {
	source, err := clusterKubeconfigPath(cluster)
	if err != nil {
		return err
	}
	target, err := defaultKubeconfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("create kubeconfig directory: %w", err)
	}
	if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
		data, readErr := os.ReadFile(source)
		if readErr != nil {
			return readErr
		}
		if writeErr := atomicWrite(target, data, 0o600); writeErr != nil {
			return writeErr
		}
		return writeOutput(map[string]string{"context": "porto-" + cluster, "path": target})
	} else if err != nil {
		return fmt.Errorf("inspect kubeconfig: %w", err)
	}

	command := exec.Command("kubectl", "config", "view", "--flatten", "--raw")
	command.Env = append(os.Environ(), "KUBECONFIG="+target+string(os.PathListSeparator)+source)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("merge Kubernetes context: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if !strings.Contains(string(output), "apiVersion:") {
		return errors.New("kubectl returned an invalid merged kubeconfig")
	}
	backup := target + ".porto-backup"
	if _, err := os.Stat(backup); errors.Is(err, os.ErrNotExist) {
		current, readErr := os.ReadFile(target)
		if readErr != nil {
			return fmt.Errorf("read kubeconfig backup source: %w", readErr)
		}
		if writeErr := atomicWrite(backup, current, 0o600); writeErr != nil {
			return fmt.Errorf("backup kubeconfig: %w", writeErr)
		}
	}
	if err := atomicWrite(target, output, 0o600); err != nil {
		return fmt.Errorf("install Kubernetes context: %w", err)
	}
	return writeOutput(map[string]string{"context": "porto-" + cluster, "path": target, "backup": backup})
}

func defaultKubeconfigPath() (string, error) {
	if configured := strings.TrimSpace(os.Getenv("KUBECONFIG")); configured != "" {
		return strings.Split(configured, string(os.PathListSeparator))[0], nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".kube", "config"), nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}
