package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"time"

	"github.com/mbianchidev/porto/internal/windowscontainers"
)

func (s *Server) windowsJSON(ctx context.Context, path string, value any) error {
	client, err := s.windowsClient()
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost"+path, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("read selected native Windows backend: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("native Windows %s returned HTTP %d", path, response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 16*1024*1024))
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("native Windows returned more than one JSON response")
	}
	return nil
}

func (s *Server) syncNativeNetworking(ctx context.Context) (bool, error) {
	if s.networking == nil || runtime.GOOS != "windows" && s.windowsTransport == nil {
		return false, nil
	}
	configuration, err := s.networking.ExportRuntime()
	if err != nil {
		return false, err
	}
	if s.windowsTransport == nil {
		return windowscontainers.ConfigureNetworking(ctx, configuration)
	}
	data, err := json.Marshal(configuration)
	if err != nil {
		return false, err
	}
	defer clear(data)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/api/worker/networking", bytes.NewReader(data))
	if err != nil {
		return false, err
	}
	response, err := s.windowsTransport.RoundTrip(request)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return false, fmt.Errorf("native networking returned HTTP %d", response.StatusCode)
	}
	return true, nil
}
