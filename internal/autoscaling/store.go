package autoscaling

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type claimRecord struct {
	UID        string     `json:"uid"`
	Name       string     `json:"name"`
	Group      string     `json:"group"`
	Runtime    string     `json:"runtime"`
	NodeName   string     `json:"nodeName"`
	ProviderID string     `json:"providerID"`
	Machine    NodeConfig `json:"config"`
}

func readJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("read autoscaling state %s: %w", path, err)
	}
	return nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	// A fixed sibling staging file is safe because all writers hold the manager
	// or provider mutex; no state is ever placed in the kubeconfig root itself.
	staging := path + ".next"
	f, err := os.OpenFile(staging, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		_ = os.Remove(staging)
		return err
	}
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		_ = os.Remove(staging)
		return err
	}
	if err := os.Rename(staging, path); err != nil {
		_ = os.Remove(staging)
		return err
	}
	return nil
}
