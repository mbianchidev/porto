//go:build windows

package windowscontainers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"github.com/mbianchidev/porto/internal/networking"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

const networkingPath = "/api/worker/networking"

func ConfigureNetworking(ctx context.Context, configuration networking.RuntimeConfiguration) (bool, error) {
	i, err := Current()
	if err != nil {
		return false, err
	}
	service, err := openService(i, true)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer service.Close()
	installed, err := readInstallation(i)
	if err != nil {
		return false, err
	}
	if err := requireOwnedService(service, installed); err != nil {
		return false, err
	}
	state, err := service.Query()
	if err != nil || state.State != svc.Running {
		return false, err
	}
	client := ClientFor(installed)
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	data, err := json.Marshal(configuration)
	if err != nil {
		return false, err
	}
	defer clear(data)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost"+networkingPath, bytes.NewReader(data))
	if err != nil {
		return false, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return false, fmt.Errorf("apply native Windows networking over its owned pipe: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	if err != nil {
		return false, err
	}
	if response.StatusCode != http.StatusNoContent {
		return false, fmt.Errorf("native Windows networking returned HTTP %d: %s", response.StatusCode, networking.Redact(string(body)))
	}
	return true, nil
}

func networkEntropy(i Installation) windows.DataBlob {
	data := []byte("Porto native networking v1 " + i.OwnerSID)
	return windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}
}

func SaveNetworking(i Installation, configuration networking.RuntimeConfiguration) error {
	if err := protectedTree(i.Root); err != nil {
		return err
	}
	data, err := json.Marshal(configuration)
	if err != nil {
		return err
	}
	defer clear(data)
	if len(data) > 2*networking.MaxRegistryCABytes {
		return errors.New("native networking configuration exceeds its bounded size")
	}
	input := windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}
	entropy := networkEntropy(i)
	var output windows.DataBlob
	if err := windows.CryptProtectData(&input, nil, &entropy, 0, nil,
		windows.CRYPTPROTECT_LOCAL_MACHINE|windows.CRYPTPROTECT_UI_FORBIDDEN, &output); err != nil {
		return fmt.Errorf("encrypt native networking with Windows DPAPI: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(output.Data)))
	return writeJSON(filepath.Join(i.Root, "networking.dpapi.json"), unsafe.Slice(output.Data, output.Size))
}

func LoadNetworking(i Installation) (*networking.RuntimeConfiguration, error) {
	path := filepath.Join(i.Root, "networking.dpapi.json")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if err := protectedTree(path); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var encrypted []byte
	if len(data) > 4*networking.MaxRegistryCABytes || json.Unmarshal(data, &encrypted) != nil || len(encrypted) == 0 {
		return nil, errors.New("invalid encrypted native networking configuration")
	}
	input := windows.DataBlob{Size: uint32(len(encrypted)), Data: &encrypted[0]}
	entropy := networkEntropy(i)
	var output windows.DataBlob
	if err := windows.CryptUnprotectData(&input, nil, &entropy, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output); err != nil {
		return nil, fmt.Errorf("decrypt owned native networking: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(output.Data)))
	plain := unsafe.Slice(output.Data, output.Size)
	defer clear(plain)
	var configuration networking.RuntimeConfiguration
	decoder := json.NewDecoder(bytes.NewReader(plain))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&configuration); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("native networking must contain exactly one configuration")
	}
	return &configuration, nil
}
