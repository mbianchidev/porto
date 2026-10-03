package main

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
	"strconv"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/dataops"
)

func uploadLocalVolumeArchive(localPath string) (dataops.Archive, error) {
	file, err := os.Open(localPath)
	if err != nil {
		return dataops.Archive{}, err
	}
	defer file.Close()
	if _, err := datafiles.Validate(context.Background(), file); err != nil {
		return dataops.Archive{}, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return dataops.Archive{}, err
	}
	request, err := http.NewRequest(http.MethodPut, "http://"+config.DaemonAddr+"/api/data/archives", file)
	if err != nil {
		return dataops.Archive{}, err
	}
	request.Header.Set("Content-Type", "application/octet-stream")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return dataops.Archive{}, err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		return dataops.Archive{}, readDataTransferError(response)
	}
	var archive dataops.Archive
	if err := json.NewDecoder(response.Body).Decode(&archive); err != nil {
		return archive, err
	}
	return archive, nil
}

func readDataTransferError(response *http.Response) error {
	message, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	if err != nil {
		return err
	}
	return fmt.Errorf("data transfer HTTP %d: %s", response.StatusCode, message)
}

func downloadCompletedVolumeExport(id int64, destination string) (err error) {
	for {
		var response bytes.Buffer
		if err := api("GET", "/api/data/operations/"+strconv.FormatInt(id, 10), nil, &response); err != nil {
			return err
		}
		var operation dataops.Operation
		if err := json.Unmarshal(response.Bytes(), &operation); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Operation %d: %s (%d bytes)\n", id, operation.Phase, operation.Bytes)
		if operation.Status == "running" {
			time.Sleep(time.Second)
			continue
		}
		if operation.Status != "succeeded" || operation.Result.Archive == nil {
			return fmt.Errorf("export %d %s: %s", id, operation.Status, operation.Error)
		}
		break
	}
	directory := filepath.Dir(destination)
	temporary, err := os.CreateTemp(directory, ".porto-download-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, temporary.Close(), os.Remove(temporary.Name())) }()
	response, err := http.Get("http://" + config.DaemonAddr + "/api/data/operations/" + strconv.FormatInt(id, 10) + "/archive")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		return readDataTransferError(response)
	}
	if _, err := io.Copy(temporary, response.Body); err != nil {
		return err
	}
	if _, err := datafiles.Validate(context.Background(), temporary); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := os.Link(temporary.Name(), destination); err != nil {
		return fmt.Errorf("publish downloaded archive without overwriting an existing file: %w", err)
	}
	return writeOutput(map[string]any{"operationId": id, "path": destination, "verified": true})
}
