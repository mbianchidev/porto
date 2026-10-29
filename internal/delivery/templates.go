package delivery

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var templateName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

var builtinTemplates = []Template{
	{Name: "http-application", Version: 1, Description: "Explicit image, loopback port, CPU/memory limits and named persistent storage.", Required: []string{"APP_IMAGE", "CONTAINER_PORT"}, SecretNames: []string{}, Compose: "services:\n  app:\n    image: ${APP_IMAGE:?Set an explicit image reference}\n    ports:\n      - '127.0.0.1:${HOST_PORT:-8080}:${CONTAINER_PORT:?Set the application port}'\n    cpus: 1\n    mem_limit: 256m\n    volumes:\n      - app-data:/data\nvolumes:\n  app-data: {}\n"},
	{Name: "background-worker", Version: 1, Description: "Explicit worker image with resource limits and a required secret environment variable.", Required: []string{"WORKER_IMAGE", "WORKER_TOKEN"}, SecretNames: []string{"WORKER_TOKEN"}, Compose: "services:\n  worker:\n    image: ${WORKER_IMAGE:?Set an explicit worker image}\n    environment:\n      WORKER_TOKEN: ${WORKER_TOKEN:?Set the stored worker token}\n    cpus: 1\n    mem_limit: 256m\n    restart: unless-stopped\n"},
}

func (m *Manager) Templates() ([]Template, error) {
	values := append([]Template{}, builtinTemplates...)
	entries, err := os.ReadDir(filepath.Join(m.root, "templates"))
	if errors.Is(err, os.ErrNotExist) {
		return values, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.root, "templates", entry.Name()))
		if err != nil {
			return nil, err
		}
		var template Template
		if err := json.Unmarshal(data, &template); err != nil {
			return nil, err
		}
		values = append(values, template)
	}
	slices.SortFunc(values, func(a, b Template) int {
		if order := strings.Compare(a.Name, b.Name); order != 0 {
			return order
		}
		return b.Version - a.Version
	})
	return values, nil
}

func (m *Manager) SaveTemplate(template Template) error {
	if !templateName.MatchString(template.Name) || template.Version < 1 || template.Version > 10000 || len(template.Compose) == 0 || len(template.Compose) > 1024*1024 {
		return fmt.Errorf("%w: template requires a simple name, positive version and bounded Compose definition", ErrInvalid)
	}
	for _, builtin := range builtinTemplates {
		if builtin.Name == template.Name {
			return fmt.Errorf("%w: built-in templates are immutable; save an edited copy under a new name", ErrInvalid)
		}
	}
	data, err := json.Marshal(template)
	if err != nil {
		return err
	}
	directory := filepath.Join(m.root, "templates")
	path := filepath.Join(directory, template.Name+"-"+strconv.Itoa(template.Version)+".json")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: template version already exists; use a new version", ErrConflict)
		}
		return err
	}
	_, writeErr := file.Write(data)
	result := errors.Join(writeErr, file.Sync(), file.Close())
	if result != nil {
		return errors.Join(result, os.Remove(path))
	}
	return nil
}

func (m *Manager) template(name string, version int) (Template, error) {
	templates, err := m.Templates()
	if err != nil {
		return Template{}, err
	}
	for _, template := range templates {
		if template.Name == name && template.Version == version {
			return template, nil
		}
	}
	return Template{}, errors.New("selected template/version does not exist")
}
