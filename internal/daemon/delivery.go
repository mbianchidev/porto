package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/app"
	"github.com/mbianchidev/porto/internal/compose"
	"github.com/mbianchidev/porto/internal/delivery"
	"github.com/mbianchidev/porto/internal/operations"
)

type deliveryBackend struct{ server *Server }

func (b deliveryBackend) AppliedDefinition(ctx context.Context, project app.Project) ([]byte, error) {
	integration, ok := b.server.compose.(interface {
		EffectiveDefinition(context.Context, app.Project) ([]byte, error)
	})
	if !ok {
		return nil, operations.Failure("Effective Compose configuration reporting is unavailable.")
	}
	return integration.EffectiveDefinition(ctx, project)
}

func (b deliveryBackend) Validate(ctx context.Context, project app.Project, directory string, definition []byte, variables map[string]string, profiles ...string) ([]byte, error) {
	integration, ok := b.server.compose.(interface {
		ValidateDefinition(context.Context, app.Project, string, []byte, map[string]string, ...string) ([]byte, error)
	})
	if !ok {
		return nil, operations.Failure("Compose validation is unavailable in this runtime.")
	}
	return integration.ValidateDefinition(ctx, project, directory, definition, variables, profiles...)
}

func (b deliveryBackend) Apply(ctx context.Context, project app.Project, activate func() error, progress func(string) error) error {
	s := b.server
	settings, err := s.store.Settings(ctx)
	if err != nil {
		return err
	}
	if !settings.DockerEnabled {
		return operations.Failure("Docker is disabled; enable it before deploying the selected application.")
	}
	s.mu.Lock()
	conflict := s.settingUp[project.ID] || s.deleting[project.ID] || s.stopping[project.ID] || s.sendboxRunning[project.ID] != nil
	s.mu.Unlock()
	if conflict {
		return operations.Failure("Another project lifecycle action is active; wait for it before redeploying.")
	}
	integration, ok := s.compose.(interface {
		StopServices(context.Context, app.Project) error
		WaitHealthy(context.Context, app.Project) error
	})
	if !ok {
		return operations.Failure("Compose delivery lifecycle is unavailable in this runtime.")
	}
	containers, err := s.docker.Containers(ctx)
	if err != nil {
		return operations.Failure("Unable to verify selected Compose container ownership before deployment.")
	}
	for _, container := range containers {
		if container.ComposeProject == compose.ProjectName(project) && s.clusters != nil {
			if err := s.clusters.ProtectContainerRemoval(ctx, container.Name); err != nil {
				return operations.Failure("Managed Kubernetes containers cannot be stopped through application delivery.")
			}
		}
	}
	if err := progress("stopping only the selected project's services; retaining volumes"); err != nil {
		return err
	}
	if _, err := s.stopProject(ctx, strconv.FormatInt(project.ID, 10), false); err != nil {
		return err
	}
	existing, err := s.delivery.DefinitionPath(project)
	if err != nil {
		return err
	}
	_, hasCompose := compose.FindFile(project.Path)
	if existing != "" || hasCompose {
		if err := integration.StopServices(ctx, project); err != nil {
			return err
		}
	}
	if err := activate(); err != nil {
		return err
	}
	definition, err := s.delivery.DefinitionPath(project)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(definition)
	if err != nil {
		return err
	}
	var buildConfig struct {
		Services map[string]struct{ Build json.RawMessage }
	}
	if err := json.Unmarshal(data, &buildConfig); err != nil {
		return err
	}
	hasBuilds := false
	for _, service := range buildConfig.Services {
		if len(service.Build) > 0 && string(service.Build) != "null" {
			hasBuilds = true
		}
	}
	if hasBuilds {
		if err := progress("building selected Compose images without cache"); err != nil {
			return err
		}
		project.ComposeFile = definition
		_, err := s.setupRunner.Run(ctx, project, func(stream, line string) error {
			return s.store.AddLog(ctx, project.ID, stream, s.delivery.Redact(project.ID, line))
		})
		if err != nil {
			return operations.Failure("Selected Compose image build failed. Inspect the project's redacted build logs; persistent volumes and recorded revisions are retained.")
		}
	}
	if err := progress("starting the recorded definition through the existing project controller"); err != nil {
		return err
	}
	started, err := s.startProject(ctx, strconv.FormatInt(project.ID, 10), true)
	if err != nil {
		return operations.Failure("The selected Compose project could not start. Inspect its redacted logs and runtime status.")
	}
	health, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := integration.WaitHealthy(health, started); err != nil {
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			cleanup, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
			_, stopErr := s.stopProject(cleanup, strconv.FormatInt(project.ID, 10), false)
			servicesErr := integration.StopServices(cleanup, project)
			cancelCleanup()
			if stopErr != nil || servicesErr != nil {
				return operations.Partial("Application operation was canceled or timed out, but scoped service cleanup did not finish. Inspect this project's containers; persistent volumes were retained.")
			}
			return err
		}
		s.mu.Lock()
		if running := s.running[project.ID]; running != nil {
			running.project.Status = "crashed"
		}
		s.mu.Unlock()
		persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return errors.Join(err, s.store.SetRuntime(persist, project.ID, "crashed", started.PID, started.Port))
	}
	s.mu.Lock()
	status, pid := "stopped", 0
	if running := s.running[project.ID]; running != nil {
		running.project.Status = "running"
		status, pid = "running", running.project.PID
	}
	s.mu.Unlock()
	return s.store.SetRuntime(ctx, project.ID, status, pid, started.Port)
}

func (b deliveryBackend) PinImages(ctx context.Context, project app.Project) (map[string]string, error) {
	result := map[string]string{}
	containers, err := b.server.docker.Containers(ctx)
	if err != nil {
		return result, err
	}
	images, err := b.server.docker.Images(ctx)
	if err != nil {
		return result, err
	}
	missing := false
	for _, container := range containers {
		if container.ComposeProject != compose.ProjectName(project) {
			continue
		}
		service := container.ComposeService
		if service == "" {
			missing = true
			continue
		}
		found := false
		for _, image := range images {
			if container.ImageID == "" || image.ID != container.ImageID && image.Digest != container.ImageID {
				continue
			}
			if !strings.HasPrefix(image.Digest, "sha256:") || len(image.Digest) != 71 {
				continue
			}
			reference := image.Digest
			if image.Repository != "" && image.Repository != "<none>" {
				reference = image.Repository + "@" + image.Digest
			}
			if existing := result[service]; existing != "" && existing != reference {
				return result, errors.New("service replicas use different image identities")
			}
			result[service], found = reference, true
			break
		}
		if !found {
			missing = true
		}
	}
	if missing || len(result) == 0 {
		return result, errors.New("some service image identities are unavailable")
	}
	return result, nil
}

func (s *Server) guardProjectOperation(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.operations == nil {
			next(w, r)
			return
		}
		project, err := s.store.GetProject(r.Context(), r.PathValue("name"))
		if err != nil {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		ctx, release, err := s.operations.Reserve(r.Context(), "project:"+strconv.FormatInt(project.ID, 10))
		if err != nil {
			http.Error(w, "a project or delivery operation already owns this project", http.StatusConflict)
			return
		}
		defer release()
		next(w, r.WithContext(ctx))
	}
}

func (s *Server) deliveryRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/delivery", func(w http.ResponseWriter, _ *http.Request) {
		value, err := s.delivery.List()
		writeDeliveryResult(w, value, err)
	})
	mux.HandleFunc("GET /api/delivery/templates", func(w http.ResponseWriter, _ *http.Request) {
		value, err := s.delivery.Templates()
		writeDeliveryResult(w, value, err)
	})
	mux.HandleFunc("POST /api/delivery/templates", func(w http.ResponseWriter, r *http.Request) {
		var template delivery.Template
		if !decodeDeliveryJSON(w, r, &template) {
			return
		}
		writeDeliveryResult(w, map[string]string{"status": "saved"}, s.delivery.SaveTemplate(template))
	})
	mux.HandleFunc("POST /api/delivery/previews", s.requireRuntime("docker", func(w http.ResponseWriter, r *http.Request) {
		var request delivery.Request
		if !decodeDeliveryJSON(w, r, &request) {
			return
		}
		value, err := s.delivery.Preview(r.Context(), request)
		writeDeliveryResult(w, value, err)
	}))
	mux.HandleFunc("GET /api/delivery/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := deliveryProjectID(w, r)
		if !ok {
			return
		}
		value, err := s.delivery.Get(id)
		writeDeliveryResult(w, value, err)
	})
	mux.HandleFunc("GET /api/delivery/{id}/definition", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("reveal") != "true" {
			http.Error(w, "explicit definition reveal is required; definitions may contain inline sensitive values", http.StatusForbidden)
			return
		}
		id, ok := deliveryProjectID(w, r)
		if !ok {
			return
		}
		value, err := s.delivery.Definition(id)
		writeDeliveryResult(w, value, err)
	})
	mux.HandleFunc("GET /api/delivery/{id}/configuration", func(w http.ResponseWriter, r *http.Request) {
		id, ok := deliveryProjectID(w, r)
		if !ok {
			return
		}
		value, err := s.delivery.Configuration(id)
		writeDeliveryResult(w, value, err)
	})
	mux.HandleFunc("POST /api/delivery/{id}/deployments", s.requireRuntime("docker", func(w http.ResponseWriter, r *http.Request) {
		id, ok := deliveryProjectID(w, r)
		if !ok {
			return
		}
		var request struct {
			Preview string `json:"preview"`
			Confirm bool   `json:"confirm"`
		}
		if !decodeDeliveryJSON(w, r, &request) {
			return
		}
		if !request.Confirm {
			http.Error(w, "explicit deployment confirmation is required", http.StatusBadRequest)
			return
		}
		if s.runtimeContext == nil {
			http.Error(w, "daemon operation context is unavailable", http.StatusServiceUnavailable)
			return
		}
		value, err := s.delivery.Deploy(s.runtimeContext, id, request.Preview)
		writeDeliveryResult(w, value, err)
	}))
	mux.HandleFunc("POST /api/delivery/{id}/rollback-previews", s.requireRuntime("docker", func(w http.ResponseWriter, r *http.Request) {
		id, ok := deliveryProjectID(w, r)
		if !ok {
			return
		}
		var request struct {
			Revision string `json:"revision"`
		}
		if !decodeDeliveryJSON(w, r, &request) {
			return
		}
		value, err := s.delivery.PreviewRollback(r.Context(), id, request.Revision)
		writeDeliveryResult(w, value, err)
	}))
	mux.HandleFunc("POST /api/delivery/{id}/updates", s.requireRuntime("docker", func(w http.ResponseWriter, r *http.Request) {
		id, ok := deliveryProjectID(w, r)
		if !ok {
			return
		}
		if s.runtimeContext == nil {
			http.Error(w, "daemon operation context is unavailable", http.StatusServiceUnavailable)
			return
		}
		value, err := s.delivery.Reconcile(s.runtimeContext, id, "manual-update")
		writeDeliveryResult(w, value, err)
	}))
	mux.HandleFunc("PUT /api/delivery/{id}/automation", func(w http.ResponseWriter, r *http.Request) {
		id, ok := deliveryProjectID(w, r)
		if !ok {
			return
		}
		var request struct {
			PollSeconds    int  `json:"pollSeconds"`
			WebhookEnabled bool `json:"webhookEnabled"`
		}
		if !decodeDeliveryJSON(w, r, &request) {
			return
		}
		value, err := s.delivery.ConfigureAutomation(id, request.PollSeconds, request.WebhookEnabled)
		writeDeliveryResult(w, value, err)
	})
	mux.HandleFunc("GET /api/operations", func(w http.ResponseWriter, _ *http.Request) {
		value, err := s.operations.List()
		writeDeliveryResult(w, value, err)
	})
	mux.HandleFunc("GET /api/operations/{id}", func(w http.ResponseWriter, r *http.Request) {
		value, err := s.operations.Get(r.PathValue("id"))
		writeDeliveryResult(w, value, err)
	})
	mux.HandleFunc("POST /api/operations/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		writeDeliveryResult(w, map[string]string{"status": "canceling"}, s.operations.Cancel(r.PathValue("id")))
	})
}

func deliveryProjectID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "positive project ID required", http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

func decodeDeliveryJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 4*1024*1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		http.Error(w, "invalid or oversized delivery request", http.StatusBadRequest)
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		http.Error(w, "exactly one delivery request is required", http.StatusBadRequest)
		return false
	}
	return true
}

func writeDeliveryResult(w http.ResponseWriter, value any, err error) {
	w.Header().Set("Cache-Control", "no-store")
	if err == nil {
		writeJSON(w, value)
		return
	}
	status := http.StatusInternalServerError
	if errors.Is(err, delivery.ErrInvalid) {
		status = http.StatusBadRequest
	}
	if errors.Is(err, delivery.ErrConflict) || errors.Is(err, operations.ErrBusy) {
		status = http.StatusConflict
	}
	if errors.Is(err, os.ErrNotExist) {
		status = http.StatusNotFound
	}
	message := operations.PublicMessage(err)
	if status != http.StatusInternalServerError {
		message = fmt.Sprint(err)
	}
	http.Error(w, message, status)
}
