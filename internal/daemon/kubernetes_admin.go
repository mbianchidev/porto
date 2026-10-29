package daemon

import (
	"context"
	"errors"
	"net/http"

	"github.com/mbianchidev/porto/internal/kubeadmin"
	"github.com/mbianchidev/porto/internal/operations"
)

func adminTarget(r *http.Request) kubeadmin.Target {
	query := r.URL.Query()
	return kubeadmin.Target{Context: query.Get("context"), Namespace: query.Get("namespace"), Group: query.Get("group"), Version: query.Get("version"), Resource: query.Get("resource"), Name: query.Get("name"), UID: query.Get("uid"), ResourceVersion: query.Get("resourceVersion")}
}

func helmScope(r *http.Request) kubeadmin.HelmScope {
	return kubeadmin.HelmScope{Context: r.URL.Query().Get("context"), Namespace: r.URL.Query().Get("namespace")}
}

func (s *Server) kubernetesAdminRoutes(mux *http.ServeMux) {
	register := func(pattern string, handler http.HandlerFunc) {
		mux.HandleFunc(pattern, s.requireRuntime("kubernetes", handler))
	}
	register("GET /api/kubernetes/admin/discovery", func(w http.ResponseWriter, r *http.Request) {
		value, err := s.kubeAdmin.Discover(r.Context(), r.URL.Query().Get("context"))
		writeKubernetesAdmin(w, value, err)
	})
	register("GET /api/kubernetes/admin/resources", func(w http.ResponseWriter, r *http.Request) {
		value, err := s.kubeAdmin.List(r.Context(), adminTarget(r), r.URL.Query().Get("continue"))
		writeKubernetesAdmin(w, value, err)
	})
	register("GET /api/kubernetes/admin/resource", func(w http.ResponseWriter, r *http.Request) {
		value, err := s.kubeAdmin.Get(r.Context(), adminTarget(r), r.URL.Query().Get("reveal") == "true")
		writeKubernetesAdmin(w, value, err)
	})
	register("GET /api/kubernetes/admin/access", func(w http.ResponseWriter, r *http.Request) {
		value, err := s.kubeAdmin.Access(r.Context(), adminTarget(r))
		writeKubernetesAdmin(w, value, err)
	})
	register("GET /api/kubernetes/admin/schema", func(w http.ResponseWriter, r *http.Request) {
		value, err := s.kubeAdmin.Schema(r.Context(), adminTarget(r))
		writeKubernetesAdmin(w, value, err)
	})
	register("GET /api/kubernetes/admin/history", func(w http.ResponseWriter, r *http.Request) {
		value, err := s.kubeAdmin.RolloutHistory(r.Context(), adminTarget(r))
		writeKubernetesAdmin(w, value, err)
	})
	register("POST /api/kubernetes/admin/previews", func(w http.ResponseWriter, r *http.Request) {
		var request kubeadmin.ManifestRequest
		if !decodeDeliveryJSON(w, r, &request) {
			return
		}
		value, err := s.kubeAdmin.PreviewManifest(r.Context(), request)
		writeKubernetesAdmin(w, value, err)
	})
	register("POST /api/kubernetes/admin/apply", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Context   string `json:"context"`
			Namespace string `json:"namespace"`
			Preview   string `json:"preview"`
			Confirm   bool   `json:"confirm"`
		}
		if !decodeDeliveryJSON(w, r, &request) {
			return
		}
		if s.runtimeContext == nil {
			http.Error(w, "daemon operation context is unavailable", http.StatusServiceUnavailable)
			return
		}
		value, err := s.kubeAdmin.ApplyManifest(s.runtimeContext, request.Context, request.Namespace, request.Preview, request.Confirm)
		writeKubernetesAdmin(w, value, err)
	})
	register("POST /api/kubernetes/admin/actions", func(w http.ResponseWriter, r *http.Request) {
		var request kubeadmin.WorkloadRequest
		if !decodeDeliveryJSON(w, r, &request) {
			return
		}
		if s.runtimeContext == nil {
			http.Error(w, "daemon operation context is unavailable", http.StatusServiceUnavailable)
			return
		}
		value, err := s.kubeAdmin.Workload(s.runtimeContext, request)
		writeKubernetesAdmin(w, value, err)
	})
	register("GET /api/kubernetes/helm/repositories", func(w http.ResponseWriter, _ *http.Request) {
		value, err := s.kubeAdmin.HelmRepositories()
		writeKubernetesAdmin(w, value, err)
	})
	register("POST /api/kubernetes/helm/repositories", func(w http.ResponseWriter, r *http.Request) {
		var request kubeadmin.HelmRepositoryRequest
		if !decodeDeliveryJSON(w, r, &request) {
			return
		}
		value, err := s.kubeAdmin.SaveHelmRepository(request)
		writeKubernetesAdmin(w, value, err)
	})
	register("DELETE /api/kubernetes/helm/repositories/{name}", func(w http.ResponseWriter, r *http.Request) {
		writeKubernetesAdmin(w, map[string]string{"status": "removed"}, s.kubeAdmin.DeleteHelmRepository(r.PathValue("name")))
	})
	register("POST /api/kubernetes/helm/repositories/update", func(w http.ResponseWriter, _ *http.Request) {
		if s.runtimeContext == nil {
			http.Error(w, "daemon operation context is unavailable", http.StatusServiceUnavailable)
			return
		}
		value, err := s.kubeAdmin.UpdateHelmRepositories(s.runtimeContext)
		writeKubernetesAdmin(w, value, err)
	})
	register("PUT /api/kubernetes/helm/charts", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 16*1024*1024)
		id, err := s.kubeAdmin.UploadHelmChart(r.Body)
		writeKubernetesAdmin(w, map[string]string{"chartId": id}, err)
	})
	register("GET /api/kubernetes/helm/releases", func(w http.ResponseWriter, r *http.Request) {
		value, err := s.kubeAdmin.HelmReleases(r.Context(), helmScope(r))
		writeKubernetesAdmin(w, value, err)
	})
	register("GET /api/kubernetes/helm/release", func(w http.ResponseWriter, r *http.Request) {
		value, err := s.kubeAdmin.HelmRelease(r.Context(), helmScope(r), r.URL.Query().Get("release"))
		writeKubernetesAdmin(w, value, err)
	})
	register("GET /api/kubernetes/helm/history", func(w http.ResponseWriter, r *http.Request) {
		value, err := s.kubeAdmin.HelmHistory(r.Context(), helmScope(r), r.URL.Query().Get("release"))
		writeKubernetesAdmin(w, value, err)
	})
	register("GET /api/kubernetes/helm/values", func(w http.ResponseWriter, r *http.Request) {
		value, err := s.kubeAdmin.HelmValues(r.Context(), helmScope(r), r.URL.Query().Get("release"), r.URL.Query().Get("reveal") == "true")
		writeKubernetesAdmin(w, value, err)
	})
	register("POST /api/kubernetes/helm/previews", func(w http.ResponseWriter, r *http.Request) {
		var request kubeadmin.HelmRequest
		if !decodeDeliveryJSON(w, r, &request) {
			return
		}
		value, err := s.kubeAdmin.PreviewHelm(r.Context(), request)
		writeKubernetesAdmin(w, value, err)
	})
	register("POST /api/kubernetes/helm/apply", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			kubeadmin.HelmScope
			Preview            string `json:"preview"`
			Confirm            bool   `json:"confirm"`
			AcknowledgeStorage bool   `json:"acknowledgeStorage"`
		}
		if !decodeDeliveryJSON(w, r, &request) {
			return
		}
		if s.runtimeContext == nil {
			http.Error(w, "daemon operation context is unavailable", http.StatusServiceUnavailable)
			return
		}
		value, err := s.kubeAdmin.ApplyHelm(s.runtimeContext, request.HelmScope, request.Preview, request.Confirm, request.AcknowledgeStorage)
		writeKubernetesAdmin(w, value, err)
	})
}

func writeKubernetesAdmin(w http.ResponseWriter, value any, err error) {
	w.Header().Set("Cache-Control", "no-store")
	if err == nil {
		writeJSON(w, value)
		return
	}
	status, message := http.StatusInternalServerError, operations.PublicMessage(err)
	var known *kubeadmin.Error
	if errors.As(err, &known) {
		status, message = known.Code, known.Message
	}
	if errors.Is(err, operations.ErrBusy) {
		status, message = http.StatusConflict, "Another project/administration operation owns this cluster or the local queue is full."
	}
	if errors.Is(err, context.Canceled) {
		status, message = http.StatusRequestTimeout, "The administration request was canceled. Inspect current resource state before retrying."
	}
	http.Error(w, message, status)
}

func (s *Server) reserveKubernetesAdministration(ctx context.Context, names ...string) (func(), error) {
	if s.operations == nil {
		return func() {}, nil
	}
	reserved := map[string]bool{}
	releases := []func(){}
	release := func() {
		for index := len(releases) - 1; index >= 0; index-- {
			releases[index]()
		}
	}
	for _, name := range names {
		for _, contextName := range []string{"porto-" + name, "porto-k3s-" + name} {
			key := "kubernetes:" + contextName
			if reserved[key] {
				continue
			}
			_, done, err := s.operations.Reserve(ctx, key)
			if err != nil {
				release()
				return nil, err
			}
			reserved[key] = true
			releases = append(releases, done)
		}
	}
	return release, nil
}
