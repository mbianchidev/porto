package wsl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/credentials"
	"github.com/mbianchidev/porto/internal/filelock"
	"github.com/mbianchidev/porto/internal/registries"
	"github.com/zalando/go-keyring"
)

const credentialService = "dev.mbianchi.porto.wsl.docker"

type CredentialVault struct {
	root    string
	secrets credentials.Store
}

type dockerCredential struct {
	ServerURL string `json:"ServerURL,omitempty"`
	Username  string `json:"Username"`
	Secret    string `json:"Secret"`
}

func NewCredentialVault(root string) *CredentialVault {
	return &CredentialVault{root: root, secrets: credentials.System{}}
}

func credentialAccount(server string) string {
	sum := sha256.Sum256([]byte(server))
	return "docker-" + hex.EncodeToString(sum[:])
}

func (v *CredentialVault) index() ([]string, error) {
	data, err := os.ReadFile(filepath.Join(v.root, "wsl-credential-index.json"))
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var servers []string
	if err := json.Unmarshal(data, &servers); err != nil {
		return nil, fmt.Errorf("read WSL credential index: %w", err)
	}
	return servers, nil
}

func (v *CredentialVault) Handle(action string, input io.Reader, output io.Writer) (err error) {
	stateMu.Lock()
	defer stateMu.Unlock()
	lockContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	lease, err := filelock.Acquire(lockContext, filepath.Join(v.root, "wsl-credentials.lock"))
	if err != nil {
		return fmt.Errorf("lock native WSL credential index: %w", err)
	}
	defer func() { err = errors.Join(err, lease.Close()) }()
	if action == "list" {
		servers, err := v.index()
		if err != nil {
			return err
		}
		usernames := map[string]string{}
		for _, server := range servers {
			secret, err := v.secrets.Get(credentialService, credentialAccount(server))
			if err != nil {
				return fmt.Errorf("read indexed WSL credential: %w", err)
			}
			var credential dockerCredential
			if err := json.Unmarshal([]byte(secret), &credential); err != nil {
				return errors.New("native WSL Docker credential is malformed; log in again")
			}
			usernames[server] = credential.Username
		}
		return json.NewEncoder(output).Encode(usernames)
	}
	data, err := io.ReadAll(io.LimitReader(input, 1024*1024+1))
	if err != nil {
		return err
	}
	if len(data) > 1024*1024 {
		return errors.New("Docker credential input exceeds one MiB")
	}
	server := strings.TrimSpace(string(data))
	var credential dockerCredential
	if action == "store" {
		if err := json.Unmarshal(data, &credential); err != nil {
			return errors.New("Docker credential must be a valid credential-helper JSON object")
		}
		if credential.Username == "" || credential.Secret == "" || strings.ContainsRune(credential.Username+credential.Secret, '\x00') {
			return errors.New("Docker credential username and secret are required and must not contain NUL")
		}
		server = credential.ServerURL
	}
	server, err = registries.NormalizeServer(server)
	if err != nil {
		return err
	}
	account := credentialAccount(server)
	if action == "get" {
		secret, err := v.secrets.Get(credentialService, account)
		if err != nil {
			return fmt.Errorf("read Docker credentials from the native Windows vault: %w", err)
		}
		var credential dockerCredential
		if err := json.Unmarshal([]byte(secret), &credential); err != nil {
			return errors.New("native WSL Docker credential is malformed; log in again")
		}
		return json.NewEncoder(output).Encode(credential)
	}
	if action != "store" && action != "erase" {
		return errors.New("Docker credential action must be get, store, erase, or list")
	}
	servers, err := v.index()
	if err != nil {
		return err
	}
	old, err := v.secrets.Get(credentialService, account)
	if err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return err
	}
	if action == "store" {
		credential.ServerURL = server
		secret, err := json.Marshal(credential)
		if err != nil {
			return err
		}
		if err := v.secrets.Set(credentialService, account, string(secret)); err != nil {
			return fmt.Errorf("store Docker credentials in the native Windows vault: %w", err)
		}
		if !slices.Contains(servers, server) {
			servers = append(servers, server)
		}
	} else {
		if err := v.secrets.Delete(credentialService, account); err != nil && !errors.Is(err, keyring.ErrNotFound) {
			return err
		}
		servers = slices.DeleteFunc(servers, func(existing string) bool { return existing == server })
	}
	if err := writeJSON(v.root, "wsl-credential-index.json", servers); err != nil {
		var restoreErr error
		if old != "" {
			restoreErr = v.secrets.Set(credentialService, account, old)
		} else {
			restoreErr = v.secrets.Delete(credentialService, account)
			if errors.Is(restoreErr, keyring.ErrNotFound) {
				restoreErr = nil
			}
		}
		return errors.Join(err, restoreErr)
	}
	return nil
}
