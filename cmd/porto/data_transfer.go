package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
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
	operation, err := waitDataOperation(id)
	if err != nil {
		return err
	}
	if operation.Result.Archive == nil {
		return fmt.Errorf("export %d completed without a verified archive", id)
	}
	available, err := datafiles.AvailableSpace(filepath.Dir(destination))
	if err != nil || uint64(operation.Result.Archive.Bytes) > available {
		return errors.Join(errors.New("insufficient local space for the verified export"), err)
	}
	directory := filepath.Dir(destination)
	temporary, err := os.CreateTemp(directory, ".porto-download-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, temporary.Close(), os.Remove(temporary.Name())) }()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+config.DaemonAddr+"/api/data/operations/"+strconv.FormatInt(id, 10)+"/archive", nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		return readDataTransferError(response)
	}
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(temporary, hash), response.Body); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != operation.Result.Archive.SHA256 {
		return errors.New("downloaded volume archive checksum does not match the verified operation; no local file was published")
	}
	if _, err := datafiles.Validate(ctx, temporary); err != nil {
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

func watchDataOperation(id int64) error {
	operation, err := waitDataOperation(id)
	if outputErr := writeOutput(operation); outputErr != nil {
		return errors.Join(err, outputErr)
	}
	return err
}

func waitDataOperation(id int64) (dataops.Operation, error) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+config.DaemonAddr+"/api/data/operations/"+strconv.FormatInt(id, 10), nil)
		if err != nil {
			return dataops.Operation{}, err
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return dataops.Operation{}, err
		}
		if response.StatusCode >= 300 {
			readErr := readDataTransferError(response)
			return dataops.Operation{}, errors.Join(readErr, response.Body.Close())
		}
		var operation dataops.Operation
		decodeErr := json.NewDecoder(response.Body).Decode(&operation)
		if err := errors.Join(decodeErr, response.Body.Close()); err != nil {
			return dataops.Operation{}, err
		}
		fmt.Fprintf(os.Stderr, "Operation %d: %s (%d bytes)\n", id, operation.Phase, operation.Bytes)
		if operation.Status == "running" {
			select {
			case <-ctx.Done():
				return operation, fmt.Errorf("stopped watching operation %d; the job continues. Cancel it with 'porto docker operations cancel %d': %w", id, id, ctx.Err())
			case <-time.After(time.Second):
			}
			continue
		}
		if operation.Status != "succeeded" {
			return operation, fmt.Errorf("data operation %d %s: %s", id, operation.Status, operation.Error)
		}
		return operation, nil
	}
}
