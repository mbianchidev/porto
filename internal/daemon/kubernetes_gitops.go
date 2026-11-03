package daemon

import (
	"net/http"

	"github.com/mbianchidev/porto/internal/kubeadmin"
)

func gitOpsScope(r *http.Request) kubeadmin.GitOpsScope {
	query := r.URL.Query()
	return kubeadmin.GitOpsScope{Context: query.Get("context"), Namespace: query.Get("namespace"), Controller: query.Get("controller"), ControllerNamespace: query.Get("controllerNamespace")}
}

func (s *Server) kubernetesGitOpsRoutes(register func(string, http.HandlerFunc)) {
	register("GET /api/kubernetes/gitops/controller", func(w http.ResponseWriter, r *http.Request) {
		value, err := s.kubeAdmin.GitOpsDiscover(r.Context(), gitOpsScope(r))
		writeKubernetesAdmin(w, value, err)
	})
	register("GET /api/kubernetes/gitops/applications", func(w http.ResponseWriter, r *http.Request) {
		value, err := s.kubeAdmin.GitOpsApplications(r.Context(), gitOpsScope(r))
		writeKubernetesAdmin(w, value, err)
	})
	register("GET /api/kubernetes/gitops/application", func(w http.ResponseWriter, r *http.Request) {
		value, err := s.kubeAdmin.GitOpsApplication(r.Context(), gitOpsScope(r), r.URL.Query().Get("kind"), r.URL.Query().Get("name"))
		writeKubernetesAdmin(w, value, err)
	})
	register("GET /api/kubernetes/gitops/sources", func(w http.ResponseWriter, r *http.Request) {
		value, err := s.kubeAdmin.GitOpsSources(gitOpsScope(r))
		writeKubernetesAdmin(w, value, err)
	})
	register("POST /api/kubernetes/gitops/sources", func(w http.ResponseWriter, r *http.Request) {
		var request kubeadmin.GitOpsSourceRequest
		if !decodeDeliveryJSON(w, r, &request) {
			return
		}
		value, err := s.kubeAdmin.SaveGitOpsSource(request)
		writeKubernetesAdmin(w, value, err)
	})
	register("DELETE /api/kubernetes/gitops/sources/{name}", func(w http.ResponseWriter, r *http.Request) {
		err := s.kubeAdmin.DeleteGitOpsSource(gitOpsScope(r), r.PathValue("name"))
		writeKubernetesAdmin(w, map[string]string{"status": "local registration removed; native applications, credentials and controllers were not uninstalled"}, err)
	})
	register("POST /api/kubernetes/gitops/previews", func(w http.ResponseWriter, r *http.Request) {
		var request kubeadmin.GitOpsRequest
		if !decodeDeliveryJSON(w, r, &request) {
			return
		}
		value, err := s.kubeAdmin.PreviewGitOps(r.Context(), request)
		writeKubernetesAdmin(w, value, err)
	})
	register("POST /api/kubernetes/gitops/controller/previews", func(w http.ResponseWriter, r *http.Request) {
		var request kubeadmin.GitOpsRequest
		if !decodeDeliveryJSON(w, r, &request) {
			return
		}
		value, err := s.kubeAdmin.PreviewGitOpsController(r.Context(), request)
		writeKubernetesAdmin(w, value, err)
	})
	apply := func(controller bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var request struct {
				kubeadmin.GitOpsScope
				Preview         string `json:"preview"`
				Confirm         bool   `json:"confirm"`
				AcknowledgeData bool   `json:"acknowledgeData"`
			}
			if !decodeDeliveryJSON(w, r, &request) {
				return
			}
			if s.runtimeContext == nil {
				http.Error(w, "daemon operation context is unavailable", http.StatusServiceUnavailable)
				return
			}
			if controller {
				value, err := s.kubeAdmin.ApplyGitOpsController(s.runtimeContext, request.GitOpsScope, request.Preview, request.Confirm, request.AcknowledgeData)
				writeKubernetesAdmin(w, value, err)
			} else {
				value, err := s.kubeAdmin.ApplyGitOps(s.runtimeContext, request.GitOpsScope, request.Preview, request.Confirm, request.AcknowledgeData)
				writeKubernetesAdmin(w, value, err)
			}
		}
	}
	register("POST /api/kubernetes/gitops/apply", apply(false))
	register("POST /api/kubernetes/gitops/controller/apply", apply(true))
}
