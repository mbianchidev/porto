package kubernetes

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/runtimes"
)

const (
	defaultTimeout = 30 * time.Second
	maxFileBytes   = 1024 * 1024
)

type Manager struct {
	runner         runtimes.Runner
	timeout        time.Duration
	kubeconfigRoot string
}

type Status struct {
	Enabled       bool   `json:"enabled"`
	Available     bool   `json:"available"`
	Context       string `json:"context"`
	ClientVersion string `json:"clientVersion,omitempty"`
	ServerVersion string `json:"serverVersion,omitempty"`
	Message       string `json:"message,omitempty"`
}

type ContextInfo struct {
	Name      string `json:"name"`
	Cluster   string `json:"cluster"`
	AuthInfo  string `json:"authInfo"`
	Namespace string `json:"namespace"`
	Current   bool   `json:"current"`
}

type Container struct {
	Name         string `json:"name"`
	Image        string `json:"image"`
	Ready        bool   `json:"ready"`
	RestartCount int32  `json:"restartCount"`
	State        string `json:"state"`
}

type Pod struct {
	Name       string      `json:"name"`
	Namespace  string      `json:"namespace"`
	Phase      string      `json:"phase"`
	Ready      string      `json:"ready"`
	Restarts   int32       `json:"restarts"`
	Node       string      `json:"node"`
	IP         string      `json:"ip"`
	Age        string      `json:"age"`
	Containers []Container `json:"containers"`
}

type PodDetail struct {
	Pod Pod             `json:"pod"`
	Raw json.RawMessage `json:"raw"`
}

type ServicePort struct {
	Name       string `json:"name"`
	Protocol   string `json:"protocol"`
	Port       int32  `json:"port"`
	TargetPort string `json:"targetPort"`
	NodePort   int32  `json:"nodePort,omitempty"`
	LocalPort  int    `json:"localPort,omitempty"`
}

type Service struct {
	Name        string        `json:"name"`
	Namespace   string        `json:"namespace"`
	Type        string        `json:"type"`
	ClusterIP   string        `json:"clusterIP"`
	ExternalIPs []string      `json:"externalIPs"`
	Ports       []ServicePort `json:"ports"`
	Age         string        `json:"age"`
}

type Node struct {
	Name          string            `json:"name"`
	Ready         bool              `json:"ready"`
	Roles         []string          `json:"roles"`
	Version       string            `json:"version"`
	InternalIP    string            `json:"internalIP"`
	Architecture  string            `json:"architecture"`
	Capacity      map[string]string `json:"capacity"`
	Allocatable   map[string]string `json:"allocatable"`
	Unschedulable bool              `json:"unschedulable"`
	Age           string            `json:"age"`
}

type Event struct {
	Type      string `json:"type"`
	Reason    string `json:"reason"`
	Message   string `json:"message"`
	Source    string `json:"source"`
	Count     int32  `json:"count"`
	FirstSeen string `json:"firstSeen"`
	LastSeen  string `json:"lastSeen"`
}

type PodStats struct {
	Container string `json:"container"`
	CPU       string `json:"cpu"`
	Memory    string `json:"memory"`
}

type FileEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

type FileListing struct {
	Path    string      `json:"path"`
	Entries []FileEntry `json:"entries"`
}

type FileContent struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
}

func New(runner runtimes.Runner) *Manager {
	return NewWithKubeconfigRoot(runner, "")
}

func NewWithKubeconfigRoot(runner runtimes.Runner, kubeconfigRoot string) *Manager {
	if runner == nil {
		runner = runtimes.ExecRunner{}
	}
	return &Manager{runner: runner, timeout: defaultTimeout, kubeconfigRoot: kubeconfigRoot}
}

func (m *Manager) Status(ctx context.Context, contextName string) Status {
	status := Status{Context: contextName}
	if status.Context == "" {
		contexts, err := m.Contexts(ctx)
		if err != nil {
			status.Message = err.Error()
			return status
		}
		if len(contexts) == 0 {
			status.Message = "No Porto-managed Kubernetes cluster exists. Create one with kind, k0s, or k3s."
			return status
		}
		status.Context = contexts[0].Name
	}
	output, err := m.run(ctx, status.Context, 15*time.Second, nil, "version", "-o", "json")
	if err != nil {
		status.Message = err.Error()
		return status
	}
	var version struct {
		ClientVersion struct {
			GitVersion string `json:"gitVersion"`
		} `json:"clientVersion"`
		ServerVersion struct {
			GitVersion string `json:"gitVersion"`
		} `json:"serverVersion"`
	}
	if err := json.Unmarshal(output, &version); err != nil {
		status.Message = fmt.Sprintf("decode Kubernetes version: %v", err)
		return status
	}
	status.Available = version.ServerVersion.GitVersion != ""
	status.ClientVersion = version.ClientVersion.GitVersion
	status.ServerVersion = version.ServerVersion.GitVersion
	if !status.Available {
		status.Message = "Kubernetes API server is unavailable"
	}
	return status
}

func (m *Manager) Contexts(ctx context.Context) ([]ContextInfo, error) {
	contextsByName := make(map[string]ContextInfo)
	if m.kubeconfigRoot != "" {
		entries, readErr := os.ReadDir(m.kubeconfigRoot)
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return nil, fmt.Errorf("list Porto kubeconfigs: %w", readErr)
		}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
				continue
			}
			managedOutput, managedErr := m.runWithKubeconfig(
				ctx,
				filepath.Join(m.kubeconfigRoot, entry.Name()),
				10*time.Second,
				nil,
				"config", "view", "-o", "json",
			)
			if managedErr != nil {
				return nil, fmt.Errorf("read managed Kubernetes context %s: %w", entry.Name(), managedErr)
			}
			var managedConfig kubeconfigView
			if err := json.Unmarshal(managedOutput, &managedConfig); err != nil {
				return nil, fmt.Errorf("decode managed Kubernetes context %s: %w", entry.Name(), err)
			}
			addContextConfig(contextsByName, managedConfig)
		}
	}
	contexts := make([]ContextInfo, 0, len(contextsByName))
	for _, item := range contextsByName {
		contexts = append(contexts, item)
	}
	sort.Slice(contexts, func(left, right int) bool { return contexts[left].Name < contexts[right].Name })
	return contexts, nil
}

func (m *Manager) Pods(ctx context.Context, contextName, namespace string) ([]Pod, error) {
	args := []string{"get", "pods"}
	if namespace == "" || namespace == "all" {
		args = append(args, "--all-namespaces")
	} else {
		args = append(args, "--namespace", namespace)
	}
	args = append(args, "-o", "json")
	output, err := m.run(ctx, contextName, m.timeout, nil, args...)
	if err != nil {
		return nil, err
	}
	var list podList
	if err := json.Unmarshal(output, &list); err != nil {
		return nil, fmt.Errorf("decode Kubernetes pods: %w", err)
	}
	pods := make([]Pod, 0, len(list.Items))
	for _, item := range list.Items {
		pods = append(pods, decodePod(item))
	}
	return pods, nil
}

func (m *Manager) Pod(ctx context.Context, contextName, namespace, name string) (PodDetail, error) {
	if err := validateResource(namespace, name); err != nil {
		return PodDetail{}, err
	}
	output, err := m.run(ctx, contextName, m.timeout, nil, "get", "pod", name, "--namespace", namespace, "-o", "json")
	if err != nil {
		return PodDetail{}, err
	}
	var item podItem
	if err := json.Unmarshal(output, &item); err != nil {
		return PodDetail{}, fmt.Errorf("decode Kubernetes pod: %w", err)
	}
	return PodDetail{Pod: decodePod(item), Raw: json.RawMessage(output)}, nil
}

func (m *Manager) Services(ctx context.Context, contextName, namespace string) ([]Service, error) {
	args := []string{"get", "services"}
	if namespace == "" || namespace == "all" {
		args = append(args, "--all-namespaces")
	} else {
		args = append(args, "--namespace", namespace)
	}
	args = append(args, "-o", "json")
	output, err := m.run(ctx, contextName, m.timeout, nil, args...)
	if err != nil {
		return nil, err
	}
	var list serviceList
	if err := json.Unmarshal(output, &list); err != nil {
		return nil, fmt.Errorf("decode Kubernetes services: %w", err)
	}
	services := make([]Service, 0, len(list.Items))
	for _, item := range list.Items {
		ports := make([]ServicePort, 0, len(item.Spec.Ports))
		for _, portItem := range item.Spec.Ports {
			ports = append(ports, ServicePort{
				Name:       portItem.Name,
				Protocol:   portItem.Protocol,
				Port:       portItem.Port,
				TargetPort: interfaceString(portItem.TargetPort),
				NodePort:   portItem.NodePort,
			})
		}
		externalIPs := append([]string(nil), item.Spec.ExternalIPs...)
		for _, ingress := range item.Status.LoadBalancer.Ingress {
			if ingress.IP != "" {
				externalIPs = append(externalIPs, ingress.IP)
			}
			if ingress.Hostname != "" {
				externalIPs = append(externalIPs, ingress.Hostname)
			}
		}
		services = append(services, Service{
			Name:        item.Metadata.Name,
			Namespace:   item.Metadata.Namespace,
			Type:        item.Spec.Type,
			ClusterIP:   item.Spec.ClusterIP,
			ExternalIPs: externalIPs,
			Ports:       ports,
			Age:         age(item.Metadata.CreationTimestamp),
		})
	}
	return services, nil
}

func (m *Manager) Nodes(ctx context.Context, contextName string) ([]Node, error) {
	output, err := m.run(ctx, contextName, m.timeout, nil, "get", "nodes", "-o", "json")
	if err != nil {
		return nil, err
	}
	var list nodeList
	if err := json.Unmarshal(output, &list); err != nil {
		return nil, fmt.Errorf("decode Kubernetes nodes: %w", err)
	}
	nodes := make([]Node, 0, len(list.Items))
	for _, item := range list.Items {
		ready := false
		for _, condition := range item.Status.Conditions {
			if condition.Type == "Ready" {
				ready = condition.Status == "True"
				break
			}
		}
		roles := make([]string, 0)
		for label := range item.Metadata.Labels {
			const prefix = "node-role.kubernetes.io/"
			if strings.HasPrefix(label, prefix) {
				roles = append(roles, strings.TrimPrefix(label, prefix))
			}
		}
		internalIP := ""
		for _, address := range item.Status.Addresses {
			if address.Type == "InternalIP" {
				internalIP = address.Address
				break
			}
		}
		nodes = append(nodes, Node{
			Name:          item.Metadata.Name,
			Ready:         ready,
			Roles:         roles,
			Version:       item.Status.NodeInfo.KubeletVersion,
			InternalIP:    internalIP,
			Architecture:  item.Status.NodeInfo.Architecture,
			Capacity:      item.Status.Capacity,
			Allocatable:   item.Status.Allocatable,
			Unschedulable: item.Spec.Unschedulable,
			Age:           age(item.Metadata.CreationTimestamp),
		})
	}
	return nodes, nil
}

func (m *Manager) Logs(ctx context.Context, contextName, namespace, pod, container string, previous bool, tail int) ([]byte, error) {
	if err := validateResource(namespace, pod); err != nil {
		return nil, err
	}
	if tail <= 0 || tail > 10000 {
		tail = 500
	}
	args := []string{"logs", pod, "--namespace", namespace, "--timestamps", "--tail", strconv.Itoa(tail)}
	if container != "" {
		args = append(args, "--container", container)
	}
	if previous {
		args = append(args, "--previous")
	}
	return m.run(ctx, contextName, 2*time.Minute, nil, args...)
}

func (m *Manager) Exec(ctx context.Context, contextName, namespace, pod, container string, command []string, stdin []byte) ([]byte, error) {
	if err := validateResource(namespace, pod); err != nil {
		return nil, err
	}
	if len(command) == 0 || strings.TrimSpace(command[0]) == "" {
		return nil, errors.New("pod command is required")
	}
	args := []string{"exec", "--namespace", namespace, pod}
	if container != "" {
		args = append(args, "--container", container)
	}
	if stdin != nil {
		args = append(args, "--stdin")
	}
	args = append(args, "--")
	args = append(args, command...)
	return m.run(ctx, contextName, 30*time.Minute, stdin, args...)
}

func (m *Manager) Files(ctx context.Context, contextName, namespace, pod, container, directory string) (FileListing, error) {
	directory, err := cleanRemotePath(directory)
	if err != nil {
		return FileListing{}, err
	}
	script := `for item in "$1"/.[!.]* "$1"/*; do
  [ -e "$item" ] || [ -L "$item" ] || continue
  if [ -L "$item" ]; then kind=symlink
  elif [ -d "$item" ]; then kind=directory
  else kind=file
  fi
  size=$(wc -c < "$item" 2>/dev/null || printf 0)
  printf '%s\t%s\t%s\n' "$kind" "$size" "${item##*/}"
done`
	output, err := m.Exec(ctx, contextName, namespace, pod, container, []string{"sh", "-c", script, "porto-files", directory}, nil)
	if err != nil {
		return FileListing{}, err
	}
	listing := FileListing{Path: directory, Entries: []FileEntry{}}
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		parts := strings.SplitN(scanner.Text(), "\t", 3)
		if len(parts) != 3 {
			continue
		}
		size, _ := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		listing.Entries = append(listing.Entries, FileEntry{Name: parts[2], Type: parts[0], Size: size})
	}
	return listing, scanner.Err()
}

func (m *Manager) ReadFile(ctx context.Context, contextName, namespace, pod, container, filePath string) (FileContent, error) {
	filePath, err := cleanRemotePath(filePath)
	if err != nil {
		return FileContent{}, err
	}
	script := `head -c "$2" "$1"`
	output, err := m.Exec(ctx, contextName, namespace, pod, container, []string{"sh", "-c", script, "porto-read", filePath, strconv.Itoa(maxFileBytes + 1)}, nil)
	if err != nil {
		return FileContent{}, err
	}
	truncated := len(output) > maxFileBytes
	if truncated {
		output = output[:maxFileBytes]
	}
	return FileContent{Path: filePath, Content: string(output), Truncated: truncated}, nil
}

func (m *Manager) WriteFile(ctx context.Context, contextName, namespace, pod, container, filePath string, content []byte) error {
	filePath, err := cleanRemotePath(filePath)
	if err != nil {
		return err
	}
	if len(content) > maxFileBytes {
		return fmt.Errorf("file content exceeds %d bytes", maxFileBytes)
	}
	script := `cat > "$1"`
	_, err = m.Exec(ctx, contextName, namespace, pod, container, []string{"sh", "-c", script, "porto-write", filePath}, content)
	return err
}

func (m *Manager) DeleteFile(ctx context.Context, contextName, namespace, pod, container, filePath string) error {
	filePath, err := cleanRemotePath(filePath)
	if err != nil {
		return err
	}
	if filePath == "/" {
		return errors.New("refusing to delete the container root filesystem")
	}
	script := `rm -rf -- "$1"`
	_, err = m.Exec(ctx, contextName, namespace, pod, container, []string{"sh", "-c", script, "porto-delete", filePath}, nil)
	return err
}

func (m *Manager) Stats(ctx context.Context, contextName, namespace, pod string) ([]PodStats, error) {
	if err := validateResource(namespace, pod); err != nil {
		return nil, err
	}
	output, err := m.run(ctx, contextName, m.timeout, nil, "top", "pod", pod, "--namespace", namespace, "--containers", "--no-headers")
	if err != nil {
		return nil, err
	}
	stats := make([]PodStats, 0)
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 {
			continue
		}
		stats = append(stats, PodStats{Container: fields[1], CPU: fields[2], Memory: fields[3]})
	}
	return stats, scanner.Err()
}

func (m *Manager) Events(ctx context.Context, contextName, namespace, pod string) ([]Event, error) {
	if err := validateResource(namespace, pod); err != nil {
		return nil, err
	}
	output, err := m.run(
		ctx,
		contextName,
		m.timeout,
		nil,
		"get", "events",
		"--namespace", namespace,
		"--field-selector", "involvedObject.name="+pod,
		"-o", "json",
	)
	if err != nil {
		return nil, err
	}
	var list eventList
	if err := json.Unmarshal(output, &list); err != nil {
		return nil, fmt.Errorf("decode Kubernetes events: %w", err)
	}
	events := make([]Event, 0, len(list.Items))
	for _, item := range list.Items {
		events = append(events, Event{
			Type:      item.Type,
			Reason:    item.Reason,
			Message:   item.Message,
			Source:    item.Source.Component,
			Count:     item.Count,
			FirstSeen: item.FirstTimestamp,
			LastSeen:  firstNonEmpty(item.LastTimestamp, item.EventTime),
		})
	}
	return events, nil
}

func (m *Manager) Manifest(ctx context.Context, contextName, namespace, pod string) ([]byte, error) {
	if err := validateResource(namespace, pod); err != nil {
		return nil, err
	}
	return m.run(ctx, contextName, m.timeout, nil, "get", "pod", pod, "--namespace", namespace, "-o", "yaml")
}

func (m *Manager) run(
	ctx context.Context,
	contextName string,
	timeout time.Duration,
	stdin []byte,
	args ...string,
) ([]byte, error) {
	if contextName == "" && (len(args) == 0 || args[0] != "config") {
		return nil, errors.New("no Porto-managed Kubernetes context is selected")
	}
	args = m.CommandArgs(contextName, args...)
	commandContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	output, err := m.runner.Run(commandContext, runtimes.Command{Name: "kubectl", Args: args, Stdin: stdin})
	if errors.Is(commandContext.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("kubectl %s timed out after %s", strings.Join(args, " "), timeout)
	}
	if err != nil {
		return nil, runtimes.CommandError("kubectl "+strings.Join(args, " "), output, err)
	}
	return output, nil
}

func (m *Manager) CommandArgs(contextName string, args ...string) []string {
	if contextName == "" {
		return args
	}
	if kubeconfigPath := m.kubeconfigForContext(contextName); kubeconfigPath != "" {
		return append([]string{"--kubeconfig", kubeconfigPath, "--context", contextName}, args...)
	}
	return append([]string{"--context", contextName}, args...)
}

func (m *Manager) runWithKubeconfig(
	ctx context.Context,
	kubeconfigPath string,
	timeout time.Duration,
	stdin []byte,
	args ...string,
) ([]byte, error) {
	args = append([]string{"--kubeconfig", kubeconfigPath}, args...)
	commandContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	output, err := m.runner.Run(commandContext, runtimes.Command{Name: "kubectl", Args: args, Stdin: stdin})
	if errors.Is(commandContext.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("kubectl %s timed out after %s", strings.Join(args, " "), timeout)
	}
	if err != nil {
		return nil, runtimes.CommandError("kubectl "+strings.Join(args, " "), output, err)
	}
	return output, nil
}

func (m *Manager) kubeconfigForContext(contextName string) string {
	const prefix = "porto-"
	if m.kubeconfigRoot == "" || !strings.HasPrefix(contextName, prefix) {
		return ""
	}
	cluster := strings.TrimPrefix(contextName, prefix)
	if cluster == "" || strings.ContainsAny(cluster, `/\\`+"\x00\r\n") {
		return ""
	}
	kubeconfigPath := filepath.Join(m.kubeconfigRoot, config.KubernetesClusterFileToken(cluster)+".yaml")
	if info, err := os.Stat(kubeconfigPath); err == nil && !info.IsDir() {
		return kubeconfigPath
	}
	return ""
}

type kubeconfigView struct {
	CurrentContext string `json:"current-context"`
	Contexts       []struct {
		Name    string `json:"name"`
		Context struct {
			Cluster   string `json:"cluster"`
			AuthInfo  string `json:"user"`
			Namespace string `json:"namespace"`
		} `json:"context"`
	} `json:"contexts"`
}

func addContextConfig(contexts map[string]ContextInfo, config kubeconfigView) {
	for _, item := range config.Contexts {
		contexts[item.Name] = ContextInfo{
			Name:      item.Name,
			Cluster:   item.Context.Cluster,
			AuthInfo:  item.Context.AuthInfo,
			Namespace: item.Context.Namespace,
			Current:   item.Name == config.CurrentContext,
		}
	}
}

func validateResource(namespace, name string) error {
	if strings.TrimSpace(namespace) == "" || strings.TrimSpace(name) == "" {
		return errors.New("Kubernetes namespace and resource name are required")
	}
	if strings.ContainsAny(namespace+name, "\r\n\x00") || strings.HasPrefix(namespace, "-") || strings.HasPrefix(name, "-") {
		return errors.New("invalid Kubernetes namespace or resource name")
	}
	return nil
}

func cleanRemotePath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = "/"
	}
	if strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("invalid container filesystem path")
	}
	cleaned := path.Clean("/" + strings.TrimPrefix(value, "/"))
	return cleaned, nil
}

func age(created string) string {
	timestamp, err := time.Parse(time.RFC3339, created)
	if err != nil {
		return ""
	}
	duration := time.Since(timestamp)
	switch {
	case duration < time.Minute:
		return fmt.Sprintf("%ds", int(duration.Seconds()))
	case duration < time.Hour:
		return fmt.Sprintf("%dm", int(duration.Minutes()))
	case duration < 24*time.Hour:
		return fmt.Sprintf("%dh", int(duration.Hours()))
	default:
		return fmt.Sprintf("%dd", int(duration.Hours()/24))
	}
}

func decodePod(item podItem) Pod {
	containersByName := make(map[string]podContainerStatus, len(item.Status.ContainerStatuses))
	for _, status := range item.Status.ContainerStatuses {
		containersByName[status.Name] = status
	}
	containers := make([]Container, 0, len(item.Spec.Containers))
	ready := 0
	var restarts int32
	for _, containerSpec := range item.Spec.Containers {
		status := containersByName[containerSpec.Name]
		if status.Ready {
			ready++
		}
		restarts += status.RestartCount
		containers = append(containers, Container{
			Name:         containerSpec.Name,
			Image:        containerSpec.Image,
			Ready:        status.Ready,
			RestartCount: status.RestartCount,
			State:        containerState(status.State),
		})
	}
	return Pod{
		Name:       item.Metadata.Name,
		Namespace:  item.Metadata.Namespace,
		Phase:      item.Status.Phase,
		Ready:      fmt.Sprintf("%d/%d", ready, len(item.Spec.Containers)),
		Restarts:   restarts,
		Node:       item.Spec.NodeName,
		IP:         item.Status.PodIP,
		Age:        age(item.Metadata.CreationTimestamp),
		Containers: containers,
	}
}

func containerState(state struct {
	Running *struct{} `json:"running"`
	Waiting *struct {
		Reason string `json:"reason"`
	} `json:"waiting"`
	Terminated *struct {
		Reason   string `json:"reason"`
		ExitCode int32  `json:"exitCode"`
	} `json:"terminated"`
}) string {
	switch {
	case state.Running != nil:
		return "running"
	case state.Waiting != nil && state.Waiting.Reason != "":
		return state.Waiting.Reason
	case state.Waiting != nil:
		return "waiting"
	case state.Terminated != nil && state.Terminated.Reason != "":
		return state.Terminated.Reason
	case state.Terminated != nil:
		return fmt.Sprintf("exited (%d)", state.Terminated.ExitCode)
	default:
		return "unknown"
	}
}

func interfaceString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatInt(int64(typed), 10)
	default:
		return fmt.Sprint(typed)
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

type metadata struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	CreationTimestamp string            `json:"creationTimestamp"`
	Labels            map[string]string `json:"labels"`
}

type podItem struct {
	Metadata metadata `json:"metadata"`
	Spec     struct {
		NodeName   string `json:"nodeName"`
		Containers []struct {
			Name  string `json:"name"`
			Image string `json:"image"`
		} `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase             string               `json:"phase"`
		PodIP             string               `json:"podIP"`
		ContainerStatuses []podContainerStatus `json:"containerStatuses"`
	} `json:"status"`
}

type podContainerStatus struct {
	Name         string `json:"name"`
	Ready        bool   `json:"ready"`
	RestartCount int32  `json:"restartCount"`
	State        struct {
		Running *struct{} `json:"running"`
		Waiting *struct {
			Reason string `json:"reason"`
		} `json:"waiting"`
		Terminated *struct {
			Reason   string `json:"reason"`
			ExitCode int32  `json:"exitCode"`
		} `json:"terminated"`
	} `json:"state"`
}

type podList struct {
	Items []podItem `json:"items"`
}

type serviceList struct {
	Items []struct {
		Metadata metadata `json:"metadata"`
		Spec     struct {
			Type        string   `json:"type"`
			ClusterIP   string   `json:"clusterIP"`
			ExternalIPs []string `json:"externalIPs"`
			Ports       []struct {
				Name       string `json:"name"`
				Protocol   string `json:"protocol"`
				Port       int32  `json:"port"`
				TargetPort any    `json:"targetPort"`
				NodePort   int32  `json:"nodePort"`
			} `json:"ports"`
		} `json:"spec"`
		Status struct {
			LoadBalancer struct {
				Ingress []struct {
					IP       string `json:"ip"`
					Hostname string `json:"hostname"`
				} `json:"ingress"`
			} `json:"loadBalancer"`
		} `json:"status"`
	} `json:"items"`
}

type nodeList struct {
	Items []struct {
		Metadata metadata `json:"metadata"`
		Spec     struct {
			Unschedulable bool `json:"unschedulable"`
		} `json:"spec"`
		Status struct {
			Capacity    map[string]string `json:"capacity"`
			Allocatable map[string]string `json:"allocatable"`
			Conditions  []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
			Addresses []struct {
				Type    string `json:"type"`
				Address string `json:"address"`
			} `json:"addresses"`
			NodeInfo struct {
				KubeletVersion string `json:"kubeletVersion"`
				Architecture   string `json:"architecture"`
			} `json:"nodeInfo"`
		} `json:"status"`
	} `json:"items"`
}

type eventList struct {
	Items []struct {
		Type      string `json:"type"`
		Reason    string `json:"reason"`
		Message   string `json:"message"`
		Count     int32  `json:"count"`
		EventTime string `json:"eventTime"`
		Source    struct {
			Component string `json:"component"`
		} `json:"source"`
		FirstTimestamp string `json:"firstTimestamp"`
		LastTimestamp  string `json:"lastTimestamp"`
	} `json:"items"`
}
