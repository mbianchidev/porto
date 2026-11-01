package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/environments"
	"github.com/mbianchidev/porto/internal/kubeadmin"
	"github.com/mbianchidev/porto/internal/kubernetes"
	"github.com/mbianchidev/porto/internal/operations"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery"
	kubeclient "k8s.io/client-go/kubernetes"
)

func (s *Server) environmentRoutes(mux *http.ServeMux) {
	register := func(pattern string, handler http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if s.environments == nil || s.environmentsErr != nil {
				err := s.environmentsErr
				if err == nil {
					err = errors.New("environment manager is unavailable")
				}
				environments.WriteError(w, err)
				return
			}
			handler(w, r)
		})
	}
	register("GET /api/environments", func(w http.ResponseWriter, r *http.Request) { environments.JSON(w, 200, s.environments.List()) })
	register("POST /api/environments", func(w http.ResponseWriter, r *http.Request) {
		var request environments.AddRequest
		if !environments.Decode(w, r, &request) {
			return
		}
		value, err := s.environments.Add(r.Context(), request)
		if err != nil {
			environments.WriteError(w, err)
			return
		}
		environments.JSON(w, 201, value)
	})
	register("POST /api/environments/{environment}/rotation", func(w http.ResponseWriter, r *http.Request) {
		if err := s.environments.Rotate(r.Context(), r.PathValue("environment")); err != nil {
			environments.WriteError(w, err)
			return
		}
		environments.JSON(w, 200, map[string]string{"status": "rotated"})
	})
	register("PUT /api/environments/{environment}/metadata", func(w http.ResponseWriter, r *http.Request) {
		var request environments.MetadataRequest
		if !environments.Decode(w, r, &request) {
			return
		}
		value, err := s.environments.Metadata(r.PathValue("environment"), request)
		if err != nil {
			environments.WriteError(w, err)
			return
		}
		environments.JSON(w, 200, value)
	})
	register("POST /api/environments/{environment}/revocation", func(w http.ResponseWriter, r *http.Request) {
		if err := s.environments.Revoke(r.Context(), r.PathValue("environment")); err != nil {
			environments.WriteError(w, err)
			return
		}
		environments.JSON(w, 200, map[string]string{"status": "revoked"})
	})
	register("DELETE /api/environments/{environment}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("environment")
		profile, err := s.environments.Profile(id)
		if err != nil {
			environments.WriteError(w, err)
			return
		}
		if profile.Backend == "kubernetes" {
			if err := s.environments.Revoke(r.Context(), id); err != nil {
				environments.WriteError(w, err)
				return
			}
			s.managementMu.Lock()
			scoped := s.environmentKube[id]
			if scoped == nil {
				root := filepath.Join(s.managementRoot, "kubernetes", id)
				scoped = &Server{operations: operations.New(filepath.Join(root, "operations")), kubeAdmin: kubeadmin.New(root, environments.KubernetesConfig{Manager: s.environments, EnvironmentID: id}, nil, s.managementVault, nil)}
			}
			closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err = scoped.operations.Close(closeCtx)
			cancel()
			if err == nil {
				err = scoped.kubeAdmin.ForgetOwnedState()
			}
			delete(s.environmentKube, id)
			s.managementMu.Unlock()
			if err != nil {
				environments.WriteError(w, err)
				return
			}
		}
		if err := s.environments.Remove(id, r.URL.Query().Get("localOnly") == "true"); err != nil {
			environments.WriteError(w, err)
			return
		}
		w.WriteHeader(204)
	})
	register("GET /api/environments/listener", func(w http.ResponseWriter, r *http.Request) { environments.JSON(w, 200, s.environments.Listener()) })
	register("DELETE /api/environments/listener", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if err := s.environments.StopListener(ctx); err != nil {
			environments.WriteError(w, err)
			return
		}
		w.WriteHeader(204)
	})
	register("POST /api/environments/listener", func(w http.ResponseWriter, r *http.Request) {
		var request environments.ListenerOptions
		if !environments.Decode(w, r, &request) {
			return
		}
		if s.runtimeContext == nil {
			environments.WriteError(w, &environments.Error{Status: 503, Code: "daemon_context", Message: "The running daemon context is unavailable."})
			return
		}
		value, err := s.environments.StartListener(s.runtimeContext, request, s.secureHandler(mux))
		if err != nil {
			environments.WriteError(w, err)
			return
		}
		environments.JSON(w, 201, value)
	})
	register("POST /api/environments/enrollments", func(w http.ResponseWriter, r *http.Request) {
		var request environments.EnrollmentRequest
		if !environments.Decode(w, r, &request) {
			return
		}
		value, err := s.environments.Enrollment(request)
		if err != nil {
			environments.WriteError(w, err)
			return
		}
		environments.JSON(w, 201, value)
	})
	register("GET /api/environments/identities", func(w http.ResponseWriter, r *http.Request) { environments.JSON(w, 200, s.environments.Peers()) })
	register("DELETE /api/environments/identities/{identity}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.environments.RevokePeer(r.PathValue("identity")); err != nil {
			environments.WriteError(w, err)
			return
		}
		w.WriteHeader(204)
	})
	register("/api/environments/{environment}/api/{resource...}", s.environmentProxy)
	register("POST /api/environments/fleet/previews", func(w http.ResponseWriter, r *http.Request) {
		var request environments.FleetRequest
		if !environments.Decode(w, r, &request) {
			return
		}
		value, err := s.fleet.Preview(r.Context(), request)
		if err != nil {
			environments.WriteError(w, err)
			return
		}
		environments.JSON(w, 201, value)
	})
	register("POST /api/environments/fleet/runs", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Preview      string `json:"preview"`
			Confirmation string `json:"confirmation"`
		}
		if !environments.Decode(w, r, &request) {
			return
		}
		if s.runtimeContext == nil {
			environments.WriteError(w, &environments.Error{Status: 503, Code: "daemon_context", Message: "The running daemon context is unavailable."})
			return
		}
		value, err := s.fleet.Start(s.runtimeContext, request.Preview, request.Confirmation)
		if err != nil {
			environments.WriteError(w, err)
			return
		}
		environments.JSON(w, 202, value)
	})
	register("GET /api/environments/fleet/runs/{run}", func(w http.ResponseWriter, r *http.Request) {
		value, err := s.fleet.Get(r.PathValue("run"))
		if err != nil {
			environments.WriteError(w, err)
			return
		}
		environments.JSON(w, 200, value)
	})
	register("GET /api/environments/fleet/runs", func(w http.ResponseWriter, r *http.Request) {
		value, err := s.fleet.List()
		if err != nil {
			environments.WriteError(w, err)
			return
		}
		environments.JSON(w, 200, value)
	})
	register("POST /api/environments/fleet/runs/{run}/cancel", func(w http.ResponseWriter, r *http.Request) {
		value, err := s.fleet.Get(r.PathValue("run"))
		if err == nil {
			err = s.operations.Cancel(value.Operation)
		}
		if err != nil {
			environments.WriteError(w, err)
			return
		}
		environments.JSON(w, 200, map[string]string{"status": "canceling", "warning": "Already dispatched effects may be unconfirmed. Inspect per-target state before retrying."})
	})
}

func (s *Server) environmentProxy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("environment")
	if header := r.Header.Get("X-Porto-Environment-ID"); header != "" && header != id {
		environments.WriteError(w, &environments.Error{Status: 409, Code: "target_mismatch", Message: "The target path and explicit environment identity disagree. No action was sent."})
		return
	}
	copy := r.Clone(r.Context())
	prefix := "/api/environments/" + id
	copy.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
	if r.URL.RawPath != "" {
		copy.URL.RawPath = strings.TrimPrefix(r.URL.RawPath, prefix)
	}
	copy.Header.Del("X-Porto-Environment-ID")
	s.serveTarget(w, copy, id)
}

func (s *Server) serveTarget(w http.ResponseWriter, r *http.Request, id string) {
	w.Header().Set("X-Porto-Environment-ID", id)
	if _, err := environments.RequiredCapability(r.Method, r.URL.EscapedPath(), r.URL.Query()); err != nil {
		environments.WriteError(w, err)
		return
	}
	if id == "local" {
		if s.coreHandler == nil {
			environments.WriteError(w, errors.New("local handler is unavailable"))
			return
		}
		s.coreHandler.ServeHTTP(w, r)
		return
	}
	if s.environments == nil || s.environmentsErr != nil {
		environments.WriteError(w, &environments.Error{Status: 503, Code: "environments_unavailable", Message: "The environment registry is unavailable; no other target was used."})
		return
	}
	profile, err := s.environments.Profile(id)
	if err != nil {
		environments.WriteError(w, err)
		return
	}
	if profile.Revoked {
		environments.WriteError(w, &environments.Error{Status: 401, Code: "revoked", Message: "This environment is revoked. Re-enroll explicitly; local fallback is forbidden."})
		return
	}
	if profile.Backend == "kubernetes" {
		s.serveImportedKubernetes(w, r, profile)
		return
	}
	if profile.Backend == "docker" && r.URL.Path == "/api/settings" && r.Method == "GET" {
		value, err := s.store.Settings(r.Context())
		if err != nil {
			environments.WriteError(w, err)
			return
		}
		value.DockerEnabled, value.KubernetesEnabled, value.VMsEnabled = true, false, false
		writeJSON(w, value)
		return
	}
	s.environments.Proxy(w, r, id)
}

type targetResponse struct {
	header    http.Header
	body      bytes.Buffer
	status    int
	oversized bool
}

func (w *targetResponse) Header() http.Header { return w.header }
func (w *targetResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *targetResponse) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	if w.body.Len()+len(data) > 1<<20 {
		w.oversized = true
		return 0, errors.New("target response exceeds 1 MiB")
	}
	return w.body.Write(data)
}
func (s *Server) targetJSON(parent context.Context, id, method, path string, body json.RawMessage) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, "http://127.0.0.1:37623"+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	response := &targetResponse{header: http.Header{}}
	s.serveTarget(response, request, id)
	if response.oversized {
		return nil, &environments.Error{Status: 413, Code: "target_response_size", Message: "Target response exceeds 1 MiB; use a narrower inventory query."}
	}
	if response.status >= 300 {
		message := strings.TrimSpace(response.body.String())
		var problem environments.Error
		if json.Unmarshal(response.body.Bytes(), &problem) == nil && problem.Message != "" {
			message = problem.Message
		}
		return nil, &environments.Error{Status: response.status, Code: "target_rejected", Message: message}
	}
	data := response.body.Bytes()
	if !json.Valid(data) {
		encoded, err := json.Marshal(map[string]string{"output": string(data)})
		return encoded, err
	}
	return slices.Clone(data), nil
}

func (s *Server) importedScope(profile environments.Profile) (*Server, error) {
	s.managementMu.Lock()
	defer s.managementMu.Unlock()
	if scoped := s.environmentKube[profile.ID]; scoped != nil {
		return scoped, nil
	}
	ctx, err := s.environments.TargetContext(profile.ID)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(s.managementRoot, "kubernetes", profile.ID)
	ops := operations.New(filepath.Join(root, "operations"))
	scoped := &Server{externalKubernetes: true, operations: ops, runtimeContext: ctx}
	scoped.kubeAdmin = kubeadmin.New(root, environments.KubernetesConfig{Manager: s.environments, EnvironmentID: profile.ID}, ops, s.managementVault, nil)
	mux := http.NewServeMux()
	scoped.kubernetesAdminRoutes(mux)
	mux.HandleFunc("GET /api/operations", func(w http.ResponseWriter, r *http.Request) {
		value, err := ops.List()
		writeDeliveryResult(w, value, err)
	})
	mux.HandleFunc("GET /api/operations/{id}", func(w http.ResponseWriter, r *http.Request) {
		value, err := ops.Get(r.PathValue("id"))
		writeDeliveryResult(w, value, err)
	})
	mux.HandleFunc("POST /api/operations/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		writeDeliveryResult(w, map[string]string{"status": "canceling"}, ops.Cancel(r.PathValue("id")))
	})
	scoped.coreHandler = mux
	s.environmentKube[profile.ID] = scoped
	return scoped, nil
}

func (s *Server) serveImportedKubernetes(w http.ResponseWriter, r *http.Request, profile environments.Profile) {
	if requested := r.URL.Query().Get("context"); requested != "" && requested != profile.Context {
		environments.WriteError(w, &environments.Error{Status: 403, Code: "wrong_context", Message: "The context must match this explicitly imported environment; no global or owned-cluster context was used."})
		return
	}
	required, err := environments.RequiredCapability(r.Method, r.URL.EscapedPath(), r.URL.Query())
	if err != nil {
		environments.WriteError(w, err)
		return
	}
	if !slices.Contains(profile.Capabilities, required) {
		environments.WriteError(w, &environments.Error{Status: 403, Code: "capability_denied", Message: "Imported environment does not grant " + required + "."})
		return
	}
	targetCtx, err := s.environments.TargetContext(profile.ID)
	if err != nil {
		environments.WriteError(w, err)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(targetCtx, cancel)
	defer func() { stop(); cancel() }()
	r = r.Clone(ctx)
	provider := environments.KubernetesConfig{Manager: s.environments, EnvironmentID: profile.ID}
	switch r.URL.Path {
	case "/api/settings":
		value, err := s.store.Settings(ctx)
		if err != nil {
			environments.WriteError(w, err)
			return
		}
		value.DockerEnabled, value.KubernetesEnabled, value.VMsEnabled = false, true, false
		writeJSON(w, value)
	case "/api/kubernetes/contexts":
		writeJSON(w, []kubernetes.ContextInfo{{Name: profile.Context, Cluster: profile.Name, AuthInfo: profile.Identity, Current: true}})
	case "/api/kubernetes/clusters":
		writeJSON(w, []kubernetes.Cluster{})
	case "/api/runtime/features":
		writeJSON(w, map[string]bool{"docker": false, "kubernetes": true, "vms": false})
	case "/api/kubernetes/status":
		if requested := r.URL.Query().Get("context"); requested != "" && requested != profile.Context {
			environments.WriteError(w, &environments.Error{Status: 403, Code: "wrong_context", Message: "Select the context explicitly imported for this environment."})
			return
		}
		config, _, err := provider.AdminConfig(profile.Context)
		if err != nil {
			environments.WriteError(w, err)
			return
		}
		client, err := discovery.NewDiscoveryClientForConfig(config)
		if err != nil {
			environments.WriteError(w, err)
			return
		}
		var version version.Info
		data, requestErr := client.RESTClient().Get().AbsPath("/version").Do(ctx).Raw()
		err = requestErr
		if err == nil {
			err = json.Unmarshal(data, &version)
		}
		if err != nil {
			s.environments.RecordHealth(profile.ID, false)
			environments.WriteError(w, &environments.Error{Status: 503, Code: "kubernetes_offline", Message: "The selected imported cluster or identity is unavailable. Verify TLS, credentials and connectivity; no global context was used."})
			return
		}
		s.environments.RecordHealth(profile.ID, true)
		writeJSON(w, kubernetes.Status{Enabled: true, Available: true, Context: profile.Context, ServerVersion: version.GitVersion})
	default:
		if strings.HasPrefix(r.URL.Path, "/api/kubernetes/admin/") || strings.HasPrefix(r.URL.Path, "/api/kubernetes/helm/") || strings.HasPrefix(r.URL.Path, "/api/operations") {
			scoped, err := s.importedScope(profile)
			if err != nil {
				environments.WriteError(w, err)
				return
			}
			scoped.coreHandler.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/kubernetes/pods/") && (strings.HasSuffix(r.URL.Path, "/terminal") || strings.HasSuffix(r.URL.Path, "/logs")) && r.Method == "GET" {
			parts := strings.Split(r.URL.Path, "/")
			if len(parts) != 7 {
				http.NotFound(w, r)
				return
			}
			config, _, err := provider.AdminConfig(profile.Context)
			if err != nil {
				environments.WriteError(w, err)
				return
			}
			if strings.ContainsAny(parts[4]+parts[5], "\x00\r\n\t ") || strings.HasPrefix(parts[4], "-") || strings.HasPrefix(parts[5], "-") || r.URL.Query().Get("uid") == "" {
				http.Error(w, "an explicit valid pod target and UID are required", 400)
				return
			}
			client, err := kubeclient.NewForConfig(config)
			if err != nil {
				environments.WriteError(w, err)
				return
			}
			pod, err := client.CoreV1().Pods(parts[4]).Get(ctx, parts[5], metav1.GetOptions{})
			if err != nil {
				code := 503
				if apierrors.IsForbidden(err) {
					code = 403
				} else if apierrors.IsUnauthorized(err) {
					code = 401
				} else if apierrors.IsNotFound(err) {
					code = 404
				}
				environments.WriteError(w, &environments.Error{Status: code, Code: "pod_identity", Message: "The selected pod identity could not be verified. Check this imported identity's get/exec/log permissions and refresh; no other context was used."})
				return
			}
			if string(pod.UID) != r.URL.Query().Get("uid") {
				environments.WriteError(w, &environments.Error{Status: 409, Code: "pod_recreated", Message: "The selected pod was replaced. Refresh its immutable identity before opening a stream or terminal."})
				return
			}
			if parts[6] == "logs" {
				tail := int64(500)
				if input := r.URL.Query().Get("tail"); input != "" {
					n, err := strconv.ParseInt(input, 10, 64)
					if err != nil || n < 1 || n > 10000 {
						http.Error(w, "tail must be 1-10000", 400)
						return
					}
					tail = n
				}
				previous := false
				if input := r.URL.Query().Get("previous"); input != "" {
					previous, err = strconv.ParseBool(input)
					if err != nil {
						http.Error(w, "previous must be true or false", 400)
						return
					}
				}
				stream, err := client.CoreV1().Pods(parts[4]).GetLogs(parts[5], &corev1.PodLogOptions{Container: r.URL.Query().Get("container"), TailLines: &tail, Previous: previous}).Stream(ctx)
				if err != nil {
					environments.WriteError(w, &environments.Error{Status: 503, Code: "pod_logs", Message: "The selected pod's logs are unavailable. Check container selection and this imported identity's log permission."})
					return
				}
				defer stream.Close()
				data, err := io.ReadAll(io.LimitReader(stream, (1<<20)+1))
				if err != nil {
					environments.WriteError(w, &environments.Error{Status: 503, Code: "pod_logs", Message: "Selected pod log transfer was interrupted. Refresh this target explicitly."})
					return
				}
				if len(data) > 1<<20 {
					http.Error(w, "selected pod logs exceed 1 MiB; select a smaller tail", 413)
					return
				}
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				if _, err := w.Write(data); err != nil {
					slog.Warn("Write imported pod logs", "environment", profile.ID, "error", err)
				}
				return
			}
			if pod.Status.Phase != corev1.PodRunning {
				http.Error(w, "selected pod must be running to open a terminal", 409)
				return
			}
			shell := r.URL.Query().Get("shell")
			if shell == "" {
				shell = "sh"
			}
			if !allowedShell(shell) {
				http.Error(w, "invalid pod shell", 400)
				return
			}
			container := r.URL.Query().Get("container")
			if strings.ContainsAny(parts[4]+parts[5]+container, "\x00\r\n\t ") || strings.HasPrefix(parts[4], "-") || strings.HasPrefix(parts[5], "-") || strings.HasPrefix(container, "-") {
				http.Error(w, "invalid imported pod target", 400)
				return
			}
			if container == "" && len(pod.Spec.Containers) > 0 {
				container = pod.Spec.Containers[0].Name
			}
			bridgeImportedPodTerminal(w, r, config, client, parts[4], parts[5], container, shell)
			return
		}
		environments.WriteError(w, &environments.Error{Status: 501, Code: "imported_capability", Message: "This imported cluster supports native resource/Helm administration and explicitly authorized pod terminals. Host provisioning, cluster lifecycle and owned-runtime APIs are not exposed."})
	}
}
