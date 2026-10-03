package docker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/mbianchidev/porto/internal/dataops"
)

func (a *API) systemDiskUsage(w http.ResponseWriter, r *http.Request) {
	usage, err := a.manager.StorageUsage(r.Context())
	if err != nil {
		writeDockerError(w, err)
		return
	}
	images, containers, volumes, cache := make([]map[string]any, 0), make([]map[string]any, 0), make([]map[string]any, 0), make([]map[string]any, 0)
	for _, item := range usage.Resources {
		switch item.Resource.Kind {
		case "image":
			images = append(images, map[string]any{
				"Id": item.Resource.ID, "RepoTags": []string{item.Resource.Name}, "Size": item.LogicalBytes,
				"SharedSize": item.SharedBytes, "Containers": len(item.Owners), "Labels": item.Labels,
			})
		case "container":
			containers = append(containers, map[string]any{
				"Id": item.Resource.ID, "Names": []string{"/" + strings.TrimPrefix(item.Resource.Name, "/")},
				"SizeRw": item.AllocatedBytes, "State": map[bool]string{true: "running", false: "exited"}[item.InUse], "Labels": item.Labels,
			})
		case "volume":
			volumes = append(volumes, map[string]any{
				"Name": item.Resource.Name, "Driver": "local", "Scope": "local", "Labels": item.Labels,
				"UsageData": map[string]any{"Size": item.AllocatedBytes, "RefCount": len(item.Owners)},
			})
		case "cache":
			cache = append(cache, map[string]any{
				"ID": item.Resource.ID, "InUse": item.InUse, "Size": item.LogicalBytes, "Shared": item.SharedBytes != nil,
			})
		}
	}
	w.Header().Set("X-Porto-Storage-Accounting", "namespace-unique-content-and-snapshots; image sizes are packed logical bytes")
	writeDockerJSON(w, http.StatusOK, map[string]any{
		"LayersSize": usage.ContentBytes, "Images": images, "Containers": containers, "Volumes": volumes, "BuildCache": cache,
		"Warnings": usage.Warnings, "PortoAccounting": usage.Accounting,
	})
}

func (a *API) pruneResources(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		filters, err := parseDockerFilters(r.URL.Query().Get("filters"))
		if err != nil {
			writeDockerError(w, err)
			return
		}
		for key := range filters {
			if !slices.Contains([]string{"label", "dangling", "until"}, key) {
				writeDockerUnsupported(w, "prune filter "+key)
				return
			}
		}
		if len(filters["until"]) > 1 {
			writeDockerUnsupported(w, "multiple prune deadlines")
			return
		}
		usage, err := a.manager.StorageUsage(r.Context())
		if err != nil {
			writeDockerError(w, err)
			return
		}
		request := dataops.Request{Action: "prune", Confirm: true, Trigger: "docker-client"}
		for _, item := range usage.Resources {
			if item.Resource.Kind != kind || !filters.matchesLabels(item.Labels) {
				continue
			}
			danglingOnly := kind == "image" && !filters["dangling"]["false"]
			if danglingOnly && !item.Dangling {
				continue
			}
			if values := filters["until"]; len(values) != 0 {
				deadline := ""
				for value := range values {
					deadline = value
				}
				until, _, err := parseDockerEventTime(deadline)
				if err != nil {
					writeDockerError(w, err)
					return
				}
				created, _, err := parseDockerEventTime(item.Resource.CreatedAt)
				if err != nil || created.IsZero() {
					writeDockerUnsupported(w, "prune age for resources without exact creation metadata")
					return
				}
				if !created.Before(until) {
					continue
				}
			}
			request.Selections = append(request.Selections, dataops.Selection{Kind: kind, Name: item.Resource.Name, ID: item.Resource.ID})
		}
		if len(request.Selections) == 0 {
			writeDockerJSON(w, http.StatusOK, pruneResponse(kind, dataops.Result{}))
			return
		}
		preview, err := a.manager.PreviewPrune(r.Context(), request)
		if err != nil {
			writeDockerError(w, err)
			return
		}
		request.Preview = preview.Token
		result, pruneErr := a.manager.Prune(r.Context(), request, func(string, int64) error { return nil })
		if a.manager.storageReporter != nil {
			if reportErr := a.manager.storageReporter(r.Context(), request, result, pruneErr); reportErr != nil {
				pruneErr = errors.Join(pruneErr, fmt.Errorf("cleanup activity could not be persisted: %w", reportErr))
			}
		}
		if pruneErr != nil {
			writeDockerError(w, pruneErr)
			return
		}
		w.Header().Set("X-Porto-Reclaimed-Bytes-Available", "false")
		writeDockerJSON(w, http.StatusOK, pruneResponse(kind, result))
	}
}

func pruneResponse(kind string, result dataops.Result) map[string]any {
	deleted := make([]string, 0)
	images := make([]map[string]string, 0)
	for _, step := range result.Steps {
		if step.Status != "succeeded" {
			continue
		}
		deleted = append(deleted, step.Source)
		if kind == "image" {
			images = append(images, map[string]string{"Untagged": step.Source})
		}
	}
	key := map[string]string{"image": "ImagesDeleted", "container": "ContainersDeleted", "volume": "VolumesDeleted", "network": "NetworksDeleted", "cache": "CachesDeleted"}[kind]
	response := map[string]any{key: deleted, "SpaceReclaimed": nil, "Warnings": []string{"Actual reclaimed bytes are unavailable across shared stores; Porto does not invent a total."}}
	if kind == "image" {
		response[key] = images
	}
	return response
}

func (m *Manager) SetStorageReporter(reporter func(context.Context, dataops.Request, dataops.Result, error) error) {
	m.storageReporter = reporter
}
