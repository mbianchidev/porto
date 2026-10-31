package daemon

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/mbianchidev/porto/internal/environments"
	"github.com/mbianchidev/porto/internal/extensions"
)

func (s *Server) extensionRoutes(mux *http.ServeMux) {
	register := func(pattern string, handler http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if s.extensions == nil || s.extensionsErr != nil {
				err := s.extensionsErr
				if err == nil {
					err = errors.New("extension host is unavailable")
				}
				environments.WriteError(w, err)
				return
			}
			handler(w, r)
		})
	}
	register("GET /api/extensions", func(w http.ResponseWriter, r *http.Request) { environments.JSON(w, 200, s.extensions.List()) })
	register("POST /api/extensions", func(w http.ResponseWriter, r *http.Request) {
		var request extensions.InstallRequest
		if !environments.Decode(w, r, &request) {
			return
		}
		value, err := s.extensions.Install(request)
		if err != nil {
			environments.WriteError(w, err)
			return
		}
		environments.JSON(w, 201, value)
	})
	register("PUT /api/extensions/{extension}/configuration", func(w http.ResponseWriter, r *http.Request) {
		var request extensions.Configuration
		if !environments.Decode(w, r, &request) {
			return
		}
		if err := s.extensions.Configure(r.PathValue("extension"), request); err != nil {
			environments.WriteError(w, err)
			return
		}
		environments.JSON(w, 200, map[string]string{"status": "configured"})
	})
	register("POST /api/extensions/{extension}/invocations", func(w http.ResponseWriter, r *http.Request) {
		var request extensions.Invocation
		if !environments.Decode(w, r, &request) {
			return
		}
		result, err := s.extensions.Invoke(r.Context(), r.PathValue("extension"), request)
		if err != nil {
			environments.WriteError(w, err)
			return
		}
		environments.JSON(w, 200, result)
	})
	register("POST /api/extensions/{extension}/cancellation", func(w http.ResponseWriter, r *http.Request) {
		if err := s.extensions.Cancel(r.PathValue("extension")); err != nil {
			environments.WriteError(w, err)
			return
		}
		environments.JSON(w, 200, map[string]string{"status": "canceling"})
	})
	register("DELETE /api/extensions/{extension}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.extensions.Remove(r.PathValue("extension")); err != nil {
			environments.WriteError(w, err)
			return
		}
		w.WriteHeader(204)
	})
	register("GET /api/extensions/{extension}/ui", func(w http.ResponseWriter, r *http.Request) {
		generation, err := strconv.ParseUint(r.URL.Query().Get("generation"), 10, 64)
		if err != nil {
			http.Error(w, "an explicit installation generation is required", 400)
			return
		}
		content, err := s.extensions.UI(r.PathValue("extension"), generation)
		if err != nil {
			environments.WriteError(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'none'; style-src 'unsafe-inline'; img-src data:; connect-src 'none'; frame-src 'none'; worker-src 'none'; base-uri 'none'; form-action 'none'; sandbox; frame-ancestors 'self'")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), display-capture=(), usb=(), serial=(), clipboard-read=(), clipboard-write=()")
		w.Header().Set("Cache-Control", "no-store")
		if _, err := w.Write([]byte(content)); err != nil {
			s.extensions.Cancel(r.PathValue("extension"))
		}
	})
	register("POST /api/extensions/{extension}/secrets", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		}
		if !environments.Decode(w, r, &request) {
			return
		}
		if err := s.extensions.SetSecret(r.PathValue("extension"), request.Name, request.Value); err != nil {
			environments.WriteError(w, err)
			return
		}
		environments.JSON(w, 201, map[string]string{"status": "stored"})
	})
	register("GET /api/extensions/publishers", func(w http.ResponseWriter, r *http.Request) { environments.JSON(w, 200, s.extensions.Publishers()) })
	register("POST /api/extensions/publishers", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			PublicKey string `json:"publicKey"`
			Label     string `json:"label"`
			Revoke    bool   `json:"revoke"`
		}
		if !environments.Decode(w, r, &request) {
			return
		}
		var err error
		if request.Revoke {
			err = s.extensions.RevokePublisher(request.PublicKey)
		} else {
			err = s.extensions.Trust(request.PublicKey, request.Label)
		}
		if err != nil {
			environments.WriteError(w, err)
			return
		}
		environments.JSON(w, 200, map[string]string{"status": "updated"})
	})
	register("GET /api/extensions/catalog", func(w http.ResponseWriter, r *http.Request) { environments.JSON(w, 200, s.extensions.Catalog()) })
	register("POST /api/extensions/catalog", func(w http.ResponseWriter, r *http.Request) {
		var request extensions.Catalog
		if !environments.Decode(w, r, &request) {
			return
		}
		if err := s.extensions.AddCatalog(request); err != nil {
			environments.WriteError(w, err)
			return
		}
		environments.JSON(w, 201, s.extensions.Catalog())
	})
	register("POST /api/extensions/catalog/install", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID        string `json:"id"`
			SHA256    string `json:"sha256"`
			Publisher string `json:"publisher"`
		}
		if !environments.Decode(w, r, &request) {
			return
		}
		value, err := s.extensions.InstallCatalog(r.Context(), request.ID, request.SHA256, request.Publisher)
		if err != nil {
			environments.WriteError(w, err)
			return
		}
		environments.JSON(w, 201, value)
	})
}
