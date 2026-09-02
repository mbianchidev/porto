package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	portodocker "github.com/mbianchidev/porto/internal/docker"
	"github.com/mbianchidev/porto/internal/kubernetes"
	"github.com/mbianchidev/porto/internal/process"
	"github.com/mbianchidev/porto/internal/resources"
)

type activityResourceSnapshot struct {
	CollectedAt time.Time               `json:"collectedAt"`
	Total       resources.Usage         `json:"total"`
	Groups      []activityResourceGroup `json:"groups"`
	Partial     bool                    `json:"partial"`
}

type activityResourceGroup struct {
	ID    string                 `json:"id"`
	Name  string                 `json:"name"`
	Total resources.Usage        `json:"total"`
	Items []activityResourceItem `json:"items"`
	Error string                 `json:"error,omitempty"`
}

type activityResourceItem struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	Detail         string          `json:"detail,omitempty"`
	Usage          resources.Usage `json:"usage"`
	CountedInTotal bool            `json:"countedInTotal"`
}

func (s *Server) activityResources(w http.ResponseWriter, r *http.Request) {
	snapshot, err := s.collectActivityResources(r.Context())
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, snapshot)
}

func (s *Server) collectActivityResources(ctx context.Context) (activityResourceSnapshot, error) {
	settings, err := s.store.Settings(ctx)
	if err != nil {
		return activityResourceSnapshot{}, fmt.Errorf("read runtime settings: %w", err)
	}
	snapshot := activityResourceSnapshot{
		CollectedAt: time.Now().UTC(),
		Groups:      make([]activityResourceGroup, 0, 5),
	}

	hostProcesses, hostProcessErr := process.CaptureResourceSnapshot(ctx)
	portoGroup := activityResourceGroup{ID: "porto", Name: "Porto", Items: make([]activityResourceItem, 0, 1)}
	if hostProcessErr != nil {
		portoGroup.Error = hostProcessErr.Error()
	} else if usage, usageErr := hostProcesses.Stats(os.Getpid(), false); usageErr != nil {
		portoGroup.Error = usageErr.Error()
	} else {
		portoGroup.Total.Add(usage)
		portoGroup.Items = append(portoGroup.Items, activityResourceItem{
			ID:             "daemon",
			Name:           "Porto daemon",
			Usage:          usage,
			CountedInTotal: true,
		})
	}
	appendResourceGroup(&snapshot, portoGroup)

	projectGroup := s.projectResourceGroup(hostProcesses, hostProcessErr)
	appendResourceGroup(&snapshot, projectGroup)

	clusters := []kubernetes.Cluster{}
	clusterErr := error(nil)
	if settings.KubernetesEnabled {
		clusters, clusterErr = s.clusters.List(ctx)
	}
	kindNodes := make(map[string]bool)
	for _, cluster := range clusters {
		if cluster.Provider == "kind" {
			for _, node := range cluster.Nodes {
				kindNodes[node] = true
			}
		}
	}

	var dockerStats []portodocker.ContainerStats
	var dockerErr error
	if settings.DockerEnabled {
		dockerStats, dockerErr = s.docker.ContainerStats(ctx)
		appendResourceGroup(&snapshot, dockerResourceGroup(dockerStats, dockerErr, kindNodes))
	}
	if settings.KubernetesEnabled {
		appendResourceGroup(&snapshot, s.kubernetesResourceGroup(ctx, clusters, clusterErr, kindNodeUsage(dockerStats, kindNodes)))
	}
	if settings.VMsEnabled {
		appendResourceGroup(&snapshot, s.vmResourceGroup(ctx))
	}
	return snapshot, nil
}

func (s *Server) projectResourceGroup(hostProcesses process.ResourceSnapshot, snapshotErr error) activityResourceGroup {
	group := activityResourceGroup{ID: "projects", Name: "Local projects", Items: make([]activityResourceItem, 0)}
	if snapshotErr != nil {
		group.Error = snapshotErr.Error()
		return group
	}
	s.mu.Lock()
	running := make([]projectProcess, 0, len(s.running))
	for _, project := range s.running {
		if project != nil {
			running = append(running, *project)
		}
	}
	s.mu.Unlock()
	var usageErrors []error
	for _, project := range running {
		if project.cmd == nil || project.cmd.Process == nil || project.stopping {
			continue
		}
		usage, err := hostProcesses.Stats(project.cmd.Process.Pid, true)
		if err != nil {
			usageErrors = append(usageErrors, fmt.Errorf("%s: %w", project.project.Name, err))
			continue
		}
		group.Total.Add(usage)
		group.Items = append(group.Items, activityResourceItem{
			ID:             strconv.FormatInt(project.project.ID, 10),
			Name:           project.project.Name,
			Detail:         project.project.Branch,
			Usage:          usage,
			CountedInTotal: true,
		})
	}
	sortResourceItems(group.Items)
	group.Error = joinedError(usageErrors)
	return group
}

func dockerResourceGroup(
	stats []portodocker.ContainerStats,
	statsErr error,
	kindNodes map[string]bool,
) activityResourceGroup {
	group := activityResourceGroup{ID: "containers", Name: "Containers", Items: make([]activityResourceItem, 0)}
	if statsErr != nil {
		group.Error = statsErr.Error()
		return group
	}
	for _, stat := range stats {
		name := strings.TrimPrefix(stat.Name, "/")
		if kindNodes[name] {
			continue
		}
		usage := resources.Usage{CPUMillicores: stat.CPUMillicores, MemoryBytes: stat.MemoryBytes}
		group.Total.Add(usage)
		group.Items = append(group.Items, activityResourceItem{
			ID:             stat.ID,
			Name:           name,
			Detail:         "container",
			Usage:          usage,
			CountedInTotal: true,
		})
	}
	sortResourceItems(group.Items)
	return group
}

func kindNodeUsage(stats []portodocker.ContainerStats, kindNodes map[string]bool) map[string]resources.Usage {
	usage := make(map[string]resources.Usage)
	for _, stat := range stats {
		name := strings.TrimPrefix(stat.Name, "/")
		if kindNodes[name] {
			usage[name] = resources.Usage{
				CPUMillicores: stat.CPUMillicores,
				MemoryBytes:   stat.MemoryBytes,
			}
		}
	}
	return usage
}

func (s *Server) kubernetesResourceGroup(
	ctx context.Context,
	clusters []kubernetes.Cluster,
	clusterErr error,
	kindUsage map[string]resources.Usage,
) activityResourceGroup {
	group := activityResourceGroup{ID: "kubernetes", Name: "Kubernetes", Items: make([]activityResourceItem, 0)}
	if clusterErr != nil {
		group.Error = clusterErr.Error()
		return group
	}
	var usageErrors []error
	for _, cluster := range clusters {
		if cluster.State != "running" && cluster.State != "degraded" {
			continue
		}
		stats, err := s.kubernetes.ResourceStats(ctx, cluster.Context)
		clusterKindUsage, completeKindUsage := kubernetesKindRuntimeUsage(cluster, kindUsage)
		if completeKindUsage {
			addKindRuntimeUsage(cluster, clusterKindUsage, &group)
		}
		if err != nil {
			usageErrors = append(usageErrors, fmt.Errorf("%s: %w", cluster.Name, err))
			if cluster.Provider == "kind" && !completeKindUsage && len(clusterKindUsage) > 0 {
				addKindRuntimeUsage(cluster, clusterKindUsage, &group)
			} else if cluster.Provider != "kind" && !completeKindUsage {
				s.addClusterVMFallback(ctx, cluster, &group, &usageErrors)
			}
			continue
		}
		if !completeKindUsage {
			group.Total.Add(stats.Total)
			for _, node := range stats.Nodes {
				group.Items = append(group.Items, activityResourceItem{
					ID:             cluster.Context + "/node/" + node.Name,
					Name:           node.Name,
					Detail:         cluster.Name + " node",
					Usage:          node.Usage,
					CountedInTotal: true,
				})
			}
		}
		for _, pod := range stats.Pods {
			group.Items = append(group.Items, activityResourceItem{
				ID:             cluster.Context + "/pod/" + pod.Namespace + "/" + pod.Pod + "/" + pod.Container,
				Name:           pod.Pod + "/" + pod.Container,
				Detail:         cluster.Name + " · " + pod.Namespace + " · included in node total",
				Usage:          pod.Usage,
				CountedInTotal: false,
			})
		}
	}
	sortResourceItems(group.Items)
	group.Error = joinedError(usageErrors)
	return group
}

func kubernetesKindRuntimeUsage(
	cluster kubernetes.Cluster,
	usage map[string]resources.Usage,
) (map[string]resources.Usage, bool) {
	if cluster.Provider != "kind" || len(cluster.Nodes) == 0 {
		return nil, false
	}
	clusterUsage := make(map[string]resources.Usage, len(cluster.Nodes))
	complete := true
	for _, node := range cluster.Nodes {
		item, ok := usage[node]
		if !ok {
			complete = false
			continue
		}
		clusterUsage[node] = item
	}
	return clusterUsage, complete
}

func addKindRuntimeUsage(
	cluster kubernetes.Cluster,
	usageByNode map[string]resources.Usage,
	group *activityResourceGroup,
) {
	for node, usage := range usageByNode {
		group.Total.Add(usage)
		group.Items = append(group.Items, activityResourceItem{
			ID:             cluster.Context + "/node/" + node,
			Name:           node,
			Detail:         cluster.Name + " kind node container",
			Usage:          usage,
			CountedInTotal: true,
		})
	}
}

func (s *Server) addClusterVMFallback(
	ctx context.Context,
	cluster kubernetes.Cluster,
	group *activityResourceGroup,
	usageErrors *[]error,
) {
	for _, node := range cluster.Nodes {
		usage, err := s.vms.ResourceStats(ctx, node)
		if err != nil {
			*usageErrors = append(*usageErrors, fmt.Errorf("%s fallback: %w", node, err))
			continue
		}
		group.Total.Add(usage)
		group.Items = append(group.Items, activityResourceItem{
			ID:             cluster.Context + "/vm/" + node,
			Name:           node,
			Detail:         cluster.Name + " VM fallback",
			Usage:          usage,
			CountedInTotal: true,
		})
	}
}

func (s *Server) vmResourceGroup(ctx context.Context) activityResourceGroup {
	group := activityResourceGroup{ID: "vms", Name: "Virtual machines", Items: make([]activityResourceItem, 0)}
	instances, err := s.vms.List(ctx)
	if err != nil {
		group.Error = err.Error()
		return group
	}
	var usageErrors []error
	for _, instance := range instances {
		if !strings.EqualFold(instance.Status, "running") {
			continue
		}
		usage, err := s.vms.ResourceStats(ctx, instance.Name)
		if err != nil {
			usageErrors = append(usageErrors, fmt.Errorf("%s: %w", instance.Name, err))
			continue
		}
		group.Total.Add(usage)
		group.Items = append(group.Items, activityResourceItem{
			ID:             instance.Name,
			Name:           instance.Name,
			Detail:         "Lima " + instance.VMType,
			Usage:          usage,
			CountedInTotal: true,
		})
	}
	sortResourceItems(group.Items)
	group.Error = joinedError(usageErrors)
	return group
}

func appendResourceGroup(snapshot *activityResourceSnapshot, group activityResourceGroup) {
	if len(group.Items) == 0 && group.Error == "" {
		return
	}
	snapshot.Groups = append(snapshot.Groups, group)
	snapshot.Total.Add(group.Total)
	snapshot.Partial = snapshot.Partial || group.Error != ""
}

func sortResourceItems(items []activityResourceItem) {
	sort.Slice(items, func(left, right int) bool {
		if items[left].Name == items[right].Name {
			return items[left].ID < items[right].ID
		}
		return items[left].Name < items[right].Name
	})
}

func joinedError(errs []error) string {
	err := errors.Join(errs...)
	if err == nil {
		return ""
	}
	return err.Error()
}
