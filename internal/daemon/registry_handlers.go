package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/app"
	portodocker "github.com/mbianchidev/porto/internal/docker"
	"github.com/mbianchidev/porto/internal/registries"
)

const (
	maxRegistryNameBytes       = 80
	maxRegistryUsernameBytes   = 256
	maxRegistryImageBytes      = 512
	maxRegistryCredentialBytes = 32 * 1024
	maxRegistryErrorBytes      = 2000
)

type registryProfileRequest struct {
	Name       string `json:"name"`
	Provider   string `json:"provider"`
	Server     string `json:"server"`
	Username   string `json:"username"`
	Credential string `json:"credential"`
	TestImage  string `json:"testImage"`
	Enabled    bool   `json:"enabled"`
}

func (s *Server) listRegistries(w http.ResponseWriter, r *http.Request) {
	s.registryMu.RLock()
	defer s.registryMu.RUnlock()
	profiles, err := s.store.ListRegistries(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, profiles)
}

func (s *Server) createRegistry(w http.ResponseWriter, r *http.Request) {
	s.registryMu.Lock()
	defer s.registryMu.Unlock()
	var request registryProfileRequest
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	profile, err := registryProfileFromRequest(request, app.RegistryProfile{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if request.Credential == "" {
		http.Error(w, "registry credential is required", http.StatusBadRequest)
		return
	}
	profile.CredentialStored = true
	created, err := s.store.CreateRegistry(r.Context(), profile)
	if err != nil {
		writeRegistryStoreError(w, err)
		return
	}
	if err := s.registryVault.Set(created.ID, request.Credential); err != nil {
		deleteErr := s.store.DeleteRegistry(r.Context(), created.ID)
		http.Error(w, errors.Join(err, deleteErr).Error(), http.StatusInternalServerError)
		return
	}
	s.invalidateRegistryConfig()
	writeJSONStatus(w, http.StatusCreated, created)
}

func (s *Server) updateRegistry(w http.ResponseWriter, r *http.Request) {
	s.registryMu.Lock()
	defer s.registryMu.Unlock()
	id, ok := registryID(w, r)
	if !ok {
		return
	}
	current, err := s.store.Registry(r.Context(), id)
	if err != nil {
		writeRegistryStoreError(w, err)
		return
	}
	var request registryProfileRequest
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	next, err := registryProfileFromRequest(request, current)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	credentialsChanged := request.Credential != ""
	connectionChanged := credentialsChanged ||
		current.Provider != next.Provider ||
		current.Server != next.Server ||
		current.Username != next.Username ||
		current.TestImage != next.TestImage
	if connectionChanged {
		next.Verified = false
		next.LastVerifiedAt = ""
		next.LastError = ""
	}

	var oldCredential string
	if credentialsChanged {
		oldCredential, err = s.registryVault.Get(id)
		if err != nil && !errors.Is(err, registries.ErrCredentialNotFound) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := s.registryVault.Set(id, request.Credential); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		next.CredentialStored = true
	}
	if err := s.store.UpdateRegistry(r.Context(), next); err != nil {
		if credentialsChanged {
			err = errors.Join(err, s.restoreRegistryCredential(id, oldCredential))
		}
		writeRegistryStoreError(w, err)
		return
	}
	s.invalidateRegistryConfig()
	updated, err := s.store.Registry(r.Context(), id)
	if err != nil {
		writeRegistryStoreError(w, err)
		return
	}
	writeJSON(w, updated)
}

func (s *Server) deleteRegistry(w http.ResponseWriter, r *http.Request) {
	s.registryMu.Lock()
	defer s.registryMu.Unlock()
	id, ok := registryID(w, r)
	if !ok {
		return
	}
	if _, err := s.store.Registry(r.Context(), id); err != nil {
		writeRegistryStoreError(w, err)
		return
	}
	credential, credentialErr := s.registryVault.Get(id)
	if credentialErr != nil && !errors.Is(credentialErr, registries.ErrCredentialNotFound) {
		http.Error(w, credentialErr.Error(), http.StatusInternalServerError)
		return
	}
	if credentialErr == nil {
		if err := s.registryVault.Delete(id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if err := s.store.DeleteRegistry(r.Context(), id); err != nil {
		if credentialErr == nil {
			err = errors.Join(err, s.registryVault.Set(id, credential))
		}
		writeRegistryStoreError(w, err)
		return
	}
	s.invalidateRegistryConfig()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) verifyRegistry(w http.ResponseWriter, r *http.Request) {
	id, ok := registryID(w, r)
	if !ok {
		return
	}
	s.registryMu.RLock()
	profile, err := s.store.Registry(r.Context(), id)
	if err != nil {
		s.registryMu.RUnlock()
		writeRegistryStoreError(w, err)
		return
	}
	credential, err := s.registryVault.Get(id)
	s.registryMu.RUnlock()
	var verificationErr error
	if err != nil {
		verificationErr = err
	} else {
		verificationErr = s.docker.PullImageWithAuth(r.Context(), profile.TestImage, "", &portodocker.RegistryAuth{
			Username:      profile.Username,
			Password:      credential,
			ServerAddress: profile.Server,
		})
	}
	verified, finishErr := s.finishRegistryVerification(r.Context(), profile, verificationErr)
	if finishErr != nil {
		if errors.Is(finishErr, errRegistryProfileChanged) {
			http.Error(w, finishErr.Error(), http.StatusConflict)
		} else {
			http.Error(w, finishErr.Error(), http.StatusInternalServerError)
		}
		return
	}
	if verificationErr != nil {
		status := http.StatusUnprocessableEntity
		if errors.Is(verificationErr, portodocker.ErrUnavailable) {
			status = http.StatusServiceUnavailable
		}
		http.Error(w, verificationErr.Error(), status)
		return
	}
	writeJSON(w, verified)
}

func registryProfileFromRequest(request registryProfileRequest, current app.RegistryProfile) (app.RegistryProfile, error) {
	name := strings.TrimSpace(request.Name)
	if name == "" {
		return app.RegistryProfile{}, errors.New("registry name is required")
	}
	if len(name) > maxRegistryNameBytes || strings.ContainsAny(name, "\x00\r\n") {
		return app.RegistryProfile{}, errors.New("registry name is invalid")
	}
	provider, err := registries.NormalizeProvider(request.Provider)
	if err != nil {
		return app.RegistryProfile{}, err
	}
	server := strings.TrimSpace(request.Server)
	if defaultServer := registries.DefaultServer(provider); defaultServer != "" {
		if server == "" {
			server = defaultServer
		}
		normalizedDefault, _ := registries.NormalizeServer(defaultServer)
		normalizedServer, normalizeErr := registries.NormalizeServer(server)
		if normalizeErr != nil {
			return app.RegistryProfile{}, normalizeErr
		}
		if normalizedServer != normalizedDefault {
			return app.RegistryProfile{}, fmt.Errorf("%s registry server must be %s", provider, normalizedDefault)
		}
		server = normalizedServer
	} else {
		server, err = registries.NormalizeServer(server)
		if err != nil {
			return app.RegistryProfile{}, err
		}
	}
	username := strings.TrimSpace(request.Username)
	if username == "" {
		return app.RegistryProfile{}, errors.New("registry username is required")
	}
	if len(username) > maxRegistryUsernameBytes || strings.ContainsAny(username, "\x00\r\n") {
		return app.RegistryProfile{}, errors.New("registry username is invalid")
	}
	if len(request.Credential) > maxRegistryCredentialBytes || strings.ContainsRune(request.Credential, '\x00') {
		return app.RegistryProfile{}, errors.New("registry credential is invalid")
	}
	testImage := strings.TrimSpace(request.TestImage)
	if testImage == "" {
		return app.RegistryProfile{}, errors.New("registry test image is required")
	}
	if len(testImage) > maxRegistryImageBytes {
		return app.RegistryProfile{}, errors.New("registry test image is too long")
	}
	if err := registries.ValidateImageServer(server, testImage); err != nil {
		return app.RegistryProfile{}, err
	}
	current.Name = name
	current.Provider = provider
	current.Server = server
	current.Username = username
	current.TestImage = testImage
	current.Enabled = request.Enabled
	return current, nil
}

func registryID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid registry profile ID", http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

func writeRegistryStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		http.Error(w, "registry profile was not found", http.StatusNotFound)
	case strings.Contains(strings.ToLower(err.Error()), "unique constraint failed"):
		http.Error(w, "a registry profile already uses this server", http.StatusConflict)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) restoreRegistryCredential(id int64, credential string) error {
	if credential == "" {
		err := s.registryVault.Delete(id)
		if errors.Is(err, registries.ErrCredentialNotFound) {
			return nil
		}
		return err
	}
	return s.registryVault.Set(id, credential)
}

var errRegistryProfileChanged = errors.New("registry profile changed during verification; run verification again")

func (s *Server) finishRegistryVerification(
	ctx context.Context,
	profile app.RegistryProfile,
	verificationErr error,
) (app.RegistryProfile, error) {
	s.registryMu.Lock()
	defer s.registryMu.Unlock()
	current, err := s.store.Registry(ctx, profile.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return app.RegistryProfile{}, errRegistryProfileChanged
		}
		return app.RegistryProfile{}, err
	}
	if current.UpdatedAt != profile.UpdatedAt {
		return app.RegistryProfile{}, errRegistryProfileChanged
	}
	verifiedAt := ""
	lastError := ""
	verified := verificationErr == nil
	if verified {
		verifiedAt = time.Now().UTC().Format(time.RFC3339Nano)
	} else if verificationErr != nil {
		lastError = strings.TrimSpace(verificationErr.Error())
		if len(lastError) > maxRegistryErrorBytes {
			lastError = lastError[:maxRegistryErrorBytes]
		}
	}
	if err := s.store.SetRegistryVerification(ctx, profile.ID, verified, verifiedAt, lastError); err != nil {
		return app.RegistryProfile{}, fmt.Errorf("save registry verification result: %w", err)
	}
	s.invalidateRegistryConfig()
	return s.store.Registry(ctx, profile.ID)
}

func (s *Server) registryAuthForImage(ctx context.Context, reference string) (*portodocker.RegistryAuth, error) {
	s.registryMu.RLock()
	defer s.registryMu.RUnlock()
	server, err := registries.ServerForImage(reference)
	if err != nil {
		return nil, err
	}
	profiles, err := s.store.ListRegistries(ctx)
	if err != nil {
		return nil, fmt.Errorf("list registry profiles: %w", err)
	}
	for _, profile := range profiles {
		if !profile.Enabled || !profile.Verified || profile.Server != server {
			continue
		}
		credential, err := s.registryVault.Get(profile.ID)
		if err != nil {
			return nil, fmt.Errorf("read credential for registry %s: %w", profile.Name, err)
		}
		return &portodocker.RegistryAuth{
			Username:      profile.Username,
			Password:      credential,
			ServerAddress: profile.Server,
		}, nil
	}
	return nil, nil
}

func (s *Server) registryDockerConfig(ctx context.Context) ([]byte, error) {
	s.registryMu.RLock()
	defer s.registryMu.RUnlock()
	profiles, err := s.store.ListRegistries(ctx)
	if err != nil {
		return nil, fmt.Errorf("list registry profiles: %w", err)
	}
	var keyBuilder strings.Builder
	active := make([]app.RegistryProfile, 0, len(profiles))
	for _, profile := range profiles {
		if !profile.Enabled || !profile.Verified {
			continue
		}
		active = append(active, profile)
		fmt.Fprintf(&keyBuilder, "%d\x00%s\x00%s\x00%s\x00", profile.ID, profile.Server, profile.Username, profile.UpdatedAt)
	}
	cacheKey := keyBuilder.String()
	s.registryConfigMu.Lock()
	if s.registryConfigKey == cacheKey {
		config := append([]byte(nil), s.registryConfig...)
		s.registryConfigMu.Unlock()
		return config, nil
	}
	s.registryConfigMu.Unlock()

	credentials := make([]registries.Credential, 0, len(active))
	for _, profile := range active {
		secret, err := s.registryVault.Get(profile.ID)
		if err != nil {
			return nil, fmt.Errorf("read credential for registry %s: %w", profile.Name, err)
		}
		credentials = append(credentials, registries.Credential{
			Server:   profile.Server,
			Username: profile.Username,
			Secret:   secret,
		})
	}
	config, err := registries.DockerConfig(credentials)
	if err != nil {
		return nil, err
	}
	s.registryConfigMu.Lock()
	s.registryConfigKey = cacheKey
	s.registryConfig = append([]byte(nil), config...)
	s.registryConfigMu.Unlock()
	return config, nil
}

func (s *Server) invalidateRegistryConfig() {
	s.registryConfigMu.Lock()
	s.registryConfigKey = ""
	s.registryConfig = nil
	s.registryConfigMu.Unlock()
}

func (s *Server) syncClusterRegistryCredentials(ctx context.Context, clusterName string) error {
	config, err := s.registryDockerConfig(ctx)
	if err != nil {
		return err
	}
	return s.clusters.SyncRegistryCredentials(ctx, clusterName, config)
}
