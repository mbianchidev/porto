package kubeadmin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/deliverysource"
	"github.com/mbianchidev/porto/internal/localtools"
	"github.com/mbianchidev/porto/internal/operations"
)

var helmName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,52}$`)

func (m *Manager) HelmRepositories() ([]HelmRepository, error) {
	data, err := os.ReadFile(filepath.Join(m.root, "helm-repositories.json"))
	if errors.Is(err, os.ErrNotExist) {
		return []HelmRepository{}, nil
	}
	if err != nil {
		return nil, err
	}
	var result []HelmRepository
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (m *Manager) SaveHelmRepository(request HelmRepositoryRequest) (HelmRepository, error) {
	if !helmName.MatchString(request.Name) || len(request.URL) > 4096 {
		return HelmRepository{}, invalid("Helm repository requires a simple lowercase name and bounded HTTPS URL.")
	}
	if err := deliverysource.ValidateHTTPS(request.URL); err != nil {
		return HelmRepository{}, invalid("Helm repository URL must use HTTPS without embedded credentials, query tokens or fragments.")
	}
	m.repoMu.Lock()
	defer m.repoMu.Unlock()
	repositories, err := m.HelmRepositories()
	if err != nil {
		return HelmRepository{}, err
	}
	value := HelmRepository{Name: request.Name, URL: request.URL}
	existing := slices.IndexFunc(repositories, func(repository HelmRepository) bool { return repository.Name == request.Name })
	if existing >= 0 {
		value.CredentialStored = repositories[existing].CredentialStored
		if repositories[existing].URL != value.URL && request.Password == nil {
			return value, conflict("A Helm source URL changed. Explicitly supply its scoped credential or an empty public credential; existing credentials are not forwarded to a new source.")
		}
	}
	if request.Password != nil {
		if len(*request.Password) > 4096 || strings.ContainsAny(*request.Password, "\r\n\x00") || len(request.Username) > 256 {
			return value, invalid("Invalid scoped Helm repository credential.")
		}
		data, _ := json.Marshal(struct{ URL, Username, Password string }{request.URL, request.Username, *request.Password})
		if err := m.vault.Set("helm-repo-"+request.Name, data); err != nil {
			return value, &Error{503, "The Helm source credential could not be saved in encrypted storage."}
		}
		value.CredentialStored = *request.Password != ""
	}
	if existing >= 0 {
		repositories[existing] = value
	} else {
		repositories = append(repositories, value)
	}
	data, err := json.Marshal(repositories)
	if err != nil {
		return value, err
	}
	return value, config.WriteFileAtomic(filepath.Join(m.root, "helm-repositories.json"), data, 0o600)
}

func (m *Manager) UpdateHelmRepositories(ctx context.Context) (operations.Operation, error) {
	return m.operations.Start(ctx, "helm:repositories", "helm.repositories.update", func(ctx context.Context, progress func(string) error) error {
		if err := progress("downloading configured HTTPS Helm repository indexes"); err != nil {
			return err
		}
		m.repoMu.Lock()
		defer m.repoMu.Unlock()
		if _, err := m.helm(ctx, HelmScope{}, nil, "repo", "update"); err != nil {
			return operations.Failure(publicError(err))
		}
		return nil
	})
}

func (m *Manager) UploadHelmChart(source io.Reader) (string, error) {
	directory := filepath.Join(m.root, "charts")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(directory, ".chart-")
	if err != nil {
		return "", err
	}
	defer func() { file.Close(); os.Remove(file.Name()) }()
	hasher := sha256.New()
	if _, err := localtools.CopyBounded(io.MultiWriter(file, hasher), source, 16*1024*1024); err != nil {
		return "", invalid("Chart archive must be at most 16 MiB.")
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	id := hex.EncodeToString(hasher.Sum(nil))
	path := filepath.Join(directory, id+".tgz")
	if err := os.Rename(file.Name(), path); err != nil {
		if _, statErr := os.Stat(path); statErr != nil {
			return "", err
		}
	}
	return id, nil
}

func chartIdentity(id string) bool {
	if len(id) != 64 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
