package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/dataops"
	portodocker "github.com/mbianchidev/porto/internal/docker"
)

func (s *Server) storageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/docker/storage", s.requireRuntime("docker", s.dockerStorage))
	mux.HandleFunc("POST /api/docker/storage/preview", s.requireRuntime("docker", s.dataPreview))
	mux.HandleFunc("GET /api/data/operations", s.dataOperations)
	mux.HandleFunc("GET /api/data/operations/{id}", s.dataOperation)
	mux.HandleFunc("POST /api/data/operations", s.requireRuntime("docker", s.createDataOperation))
	mux.HandleFunc("DELETE /api/data/operations/{id}", s.cancelDataOperation)
	mux.HandleFunc("GET /api/data/operations/{id}/archive", s.dataOperationArchive)
	mux.HandleFunc("PUT /api/data/archives", s.requireRuntime("docker", s.uploadVolumeArchive))
	mux.HandleFunc("GET /api/docker/backups", s.backupSchedules)
	mux.HandleFunc("POST /api/docker/backups", s.requireRuntime("docker", s.saveBackupSchedule))
	mux.HandleFunc("DELETE /api/docker/backups/{id}", s.deleteBackupSchedule)
	mux.HandleFunc("POST /api/docker/backups/{id}/run", s.requireRuntime("docker", s.runBackupSchedule))
	mux.HandleFunc("GET /api/docker/migration/contexts", s.requireRuntime("docker", s.migrationContexts))
	mux.HandleFunc("GET /api/docker/migration/inventory", s.requireRuntime("docker", s.migrationInventory))
}

func transferDirectory() (string, error) {
	root, err := config.Dir()
	if err != nil {
		return "", err
	}
	directory := filepath.Join(root, "transfers")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	return directory, os.Chmod(directory, 0o700)
}

func (s *Server) dockerStorage(w http.ResponseWriter, r *http.Request) {
	value, err := s.docker.StorageUsage(r.Context())
	writeRuntimeResult(w, value, err)
}

func (s *Server) dataPreview(w http.ResponseWriter, r *http.Request) {
	var request dataops.Request
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	switch request.Action {
	case "migration":
		value, err := s.docker.PreviewMigration(r.Context(), request)
		writeRuntimeResult(w, value, err)
	case "prune":
		value, err := s.docker.PreviewPrune(r.Context(), request)
		writeRuntimeResult(w, value, err)
	case "volume-export", "volume-clone", "volume-import", "volume-restore", "volume-empty":
		value, err := s.docker.PreviewVolume(r.Context(), request)
		writeRuntimeResult(w, value, err)
	default:
		writeRuntimeError(w, fmt.Errorf("%w: unknown data operation action", datafiles.ErrInvalid))
	}
}

func (s *Server) dataOperations(w http.ResponseWriter, r *http.Request) {
	runs, err := s.store.DataOperations(r.Context())
	if err == nil {
		s.dataMu.Lock()
		for index, run := range runs {
			if shadow := s.dataUnstored[run.ID]; shadow != nil {
				runs[index] = *shadow
			}
		}
		s.dataMu.Unlock()
	}
	writeRuntimeResult(w, runs, err)
}

func operationID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return 0, fmt.Errorf("%w: invalid operation ID", datafiles.ErrInvalid)
	}
	return id, nil
}

func (s *Server) dataOperation(w http.ResponseWriter, r *http.Request) {
	id, err := operationID(r)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	value, err := s.store.DataOperation(r.Context(), id)
	writeRuntimeResult(w, value, err)
}

func (s *Server) createDataOperation(w http.ResponseWriter, r *http.Request) {
	var request dataops.Request
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	if !request.Confirm || request.Preview == "" {
		writeRuntimeError(w, fmt.Errorf("%w: an explicitly confirmed data preview is required", datafiles.ErrInvalid))
		return
	}
	run, err := s.startDataOperation(r.Context(), request)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSONStatus(w, http.StatusAccepted, run)
}

func (s *Server) startDataOperation(ctx context.Context, request dataops.Request) (*dataops.Operation, error) {
	if s.runtimeContext == nil || s.runtimeContext.Err() != nil {
		return nil, fmt.Errorf("%w: data worker is not initialized or Porto is shutting down", portodocker.ErrUnavailable)
	}
	s.dataMu.Lock()
	defer s.dataMu.Unlock()
	if len(s.dataCancels) > 0 || len(s.dataUnstored) > 0 {
		return nil, fmt.Errorf("%w: a data job is running or its results could not be persisted", portodocker.ErrConflict)
	}
	if !s.beginRuntimeOperation() {
		return nil, portodocker.ErrConflict
	}
	if request.Trigger == "" {
		request.Trigger = "manual"
	}
	run, err := s.store.BeginDataOperation(ctx, request, time.Now().UTC())
	if err != nil {
		s.endRuntimeOperation()
		return nil, err
	}
	s.launchDataOperationLocked(*run)
	return run, nil
}

func (s *Server) launchDataOperationLocked(run dataops.Operation) {
	if s.dataCancels == nil {
		s.dataCancels = make(map[int64]context.CancelFunc)
	}
	if s.dataDone == nil {
		s.dataDone = make(map[int64]chan struct{})
	}
	if s.dataUnstored == nil {
		s.dataUnstored = make(map[int64]*dataops.Operation)
	}
	ctx, cancel := context.WithTimeout(s.runtimeContext, 2*time.Hour)
	done := make(chan struct{})
	s.dataCancels[run.ID], s.dataDone[run.ID] = cancel, done
	go func() {
		defer func() {
			cancel()
			s.dataMu.Lock()
			delete(s.dataCancels, run.ID)
			delete(s.dataDone, run.ID)
			close(done)
			s.dataMu.Unlock()
			s.endRuntimeOperation()
		}()
		executor := s.dataExecutor
		if executor == nil {
			executor = s.executeDataOperation
		}
		result, operationErr := executor(ctx, run)
		status := "succeeded"
		message := ""
		if operationErr != nil {
			status, message = "failed", operationErr.Error()
			if errors.Is(operationErr, context.Canceled) {
				status = "cancelled"
			}
		}
		if operationErr == nil && run.Request.ScheduleID != 0 && result.Archive != nil {
			if err := s.applyBackupRetention(ctx, run, *result.Archive); err != nil {
				status, message = "failed", "Backup verified, but retention failed: "+err.Error()
			}
		}
		persistContext, cancelPersist := context.WithTimeout(context.Background(), 10*time.Second)
		persistErr := s.store.FinishDataOperation(persistContext, run.ID, status, result, message, time.Now().UTC())
		cancelPersist()
		if persistErr != nil {
			run.Status, run.Result, run.Error, run.CompletedAt = "failed", result, errors.Join(operationErr, persistErr).Error(), time.Now().UTC().Format(time.RFC3339Nano)
			s.dataMu.Lock()
			s.dataUnstored[run.ID] = &run
			s.dataMu.Unlock()
			log.Printf("persist data operation %d: %v", run.ID, persistErr)
		}
		log.Printf("data operation %d (%s) %s", run.ID, run.Request.Action, status)
	}()
}

func (s *Server) executeDataOperation(ctx context.Context, run dataops.Operation) (result dataops.Result, err error) {
	request := run.Request
	progress := func(phase string, bytes int64) error { return s.store.DataProgress(ctx, run.ID, phase, bytes) }
	if request.Action == "prune" {
		return s.docker.Prune(ctx, request, progress)
	}
	if request.Action == "migration" {
		directory, err := transferDirectory()
		if err != nil {
			return result, err
		}
		return s.docker.Migrate(ctx, request, directory, progress)
	}
	preview, err := s.docker.PreviewVolume(ctx, request)
	if err != nil {
		return result, err
	}
	if request.ScheduleID == 0 && (request.Preview == "" || request.Preview != preview.Token) {
		return result, fmt.Errorf("%w: volume preview changed; review it again", datafiles.ErrConflict)
	}
	if request.ScheduleID != 0 && request.Identity != preview.Resource.Fingerprint() {
		return result, fmt.Errorf("%w: scheduled volume identity changed; schedule was not retargeted", datafiles.ErrConflict)
	}
	request = preview.Request
	directory, err := transferDirectory()
	if err != nil {
		return result, err
	}
	switch request.Action {
	case "volume-export":
		destination := request.Destination
		if destination == "" {
			destinationDirectory := directory
			if request.Directory != "" {
				destinationDirectory = request.Directory
			}
			destination = filepath.Join(destinationDirectory, fmt.Sprintf("volume-%s-%d.tar", request.Identity[:12], run.ID))
		}
		archive, err := s.docker.ExportVolume(ctx, preview.Resource, request.Identity, destination, progress)
		result.Archive = &archive
		return result, err
	case "volume-clone":
		return s.docker.CloneVolume(ctx, preview.Resource, request.Identity, request.Destination,
			filepath.Join(directory, fmt.Sprintf("clone-%d.tar", run.ID)), progress)
	case "volume-import":
		return s.docker.ImportVolume(ctx, request.Destination, request.Archive, preview.Archive.SHA256, progress)
	case "volume-restore", "volume-empty":
		ctx, release, err := s.docker.BeginDataTransaction(ctx)
		if err != nil {
			return result, err
		}
		defer release()
		if request.Action == "volume-restore" {
			err = s.docker.RestoreVolume(ctx, preview.Resource, request.Identity, request.Archive, preview.Archive.SHA256, progress)
		} else {
			descriptor, descriptorErr := s.docker.FileDescriptor(ctx, "volume", preview.Resource.Name)
			if descriptorErr != nil {
				return result, descriptorErr
			}
			var output bytes.Buffer
			err = s.docker.RunFileRequest(ctx, descriptor, datafiles.Request{
				Action: "empty", Identity: request.Identity, Confirm: true, SHA256: request.Archive,
			}, nil, &output)
		}
		if err == nil {
			result.Steps = []dataops.Step{{Kind: "volume", Source: preview.Resource.Name, Destination: preview.Resource.Name, ID: preview.Resource.ID, Status: "succeeded"}}
		}
		return result, err
	default:
		return result, fmt.Errorf("%w: unknown operation action", datafiles.ErrInvalid)
	}
}

func (s *Server) cancelDataOperation(w http.ResponseWriter, r *http.Request) {
	id, err := operationID(r)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	s.dataMu.Lock()
	cancel := s.dataCancels[id]
	s.dataMu.Unlock()
	if cancel == nil {
		writeRuntimeError(w, fmt.Errorf("%w: data operation is not running", portodocker.ErrConflict))
		return
	}
	cancel()
	writeJSON(w, map[string]string{"status": "cancellation-requested"})
}

func (s *Server) stopDataOperations(ctx context.Context) error {
	s.dataMu.Lock()
	var pending []chan struct{}
	for id, cancel := range s.dataCancels {
		cancel()
		pending = append(pending, s.dataDone[id])
	}
	s.dataMu.Unlock()
	for _, done := range pending {
		select {
		case <-done:
		case <-ctx.Done():
			return fmt.Errorf("wait for data transfer cleanup: %w", ctx.Err())
		}
	}
	return nil
}

func (s *Server) dataOperationArchive(w http.ResponseWriter, r *http.Request) {
	id, err := operationID(r)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	run, err := s.store.DataOperation(r.Context(), id)
	if err != nil || run.Result.Archive == nil || run.Result.Archive.Path == "" {
		http.Error(w, "Verified archive is unavailable or has expired under retention.", http.StatusNotFound)
		return
	}
	current, err := portodocker.InspectVolumeArchive(r.Context(), run.Result.Archive.Path)
	if err != nil || current.SHA256 != run.Result.Archive.SHA256 {
		writeRuntimeError(w, errors.Join(datafiles.ErrConflict, err))
		return
	}
	root, file, err := datafiles.OpenManagedArchive(current.Path)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	defer root.Close()
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Disposition", "attachment")
	http.ServeContent(w, r, filepath.Base(current.Path), info.ModTime(), file)
}

func (s *Server) uploadVolumeArchive(w http.ResponseWriter, r *http.Request) {
	directory, err := transferDirectory()
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	file, err := os.CreateTemp(directory, "upload-*.tar")
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	keep := false
	defer func() {
		closeErr := file.Close()
		if !keep {
			closeErr = errors.Join(closeErr, os.Remove(file.Name()))
		}
		if closeErr != nil {
			log.Printf("cleanup staged archive upload: %v", closeErr)
		}
	}()
	r.Body = http.MaxBytesReader(w, r.Body, datafiles.MaxArchiveBytes+32*1024*1024)
	if _, err := io.Copy(file, r.Body); err != nil {
		writeRuntimeError(w, err)
		return
	}
	if err := file.Sync(); err != nil {
		writeRuntimeError(w, err)
		return
	}
	archive, err := portodocker.InspectVolumeArchive(r.Context(), file.Name())
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	keep = true
	writeJSONStatus(w, http.StatusCreated, archive)
}

func (s *Server) backupSchedules(w http.ResponseWriter, r *http.Request) {
	value, err := s.store.BackupSchedules(r.Context())
	writeRuntimeResult(w, value, err)
}

func (s *Server) saveBackupSchedule(w http.ResponseWriter, r *http.Request) {
	var schedule dataops.Schedule
	if !decodeRuntimeJSON(w, r, &schedule) {
		return
	}
	descriptor, err := s.docker.FileDescriptor(r.Context(), "volume", schedule.Resource.Name)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	if schedule.Resource.ID != "" && schedule.Resource.Fingerprint() != descriptor.Resource.Fingerprint() {
		writeRuntimeError(w, datafiles.ErrConflict)
		return
	}
	schedule.Resource = descriptor.Resource
	if schedule.Directory == "" {
		root, err := config.Dir()
		if err != nil {
			writeRuntimeError(w, err)
			return
		}
		schedule.Directory = filepath.Join(root, "backups", schedule.Resource.Fingerprint()[:16])
	} else if !filepath.IsAbs(schedule.Directory) {
		if !filepath.IsLocal(schedule.Directory) {
			writeRuntimeError(w, datafiles.ErrInvalid)
			return
		}
		root, err := config.Dir()
		if err != nil {
			writeRuntimeError(w, err)
			return
		}
		schedule.Directory = filepath.Join(root, "backups", schedule.Directory)
	}
	if _, _, err := datafiles.ManagedLocation(schedule.Directory); err != nil {
		writeRuntimeError(w, err)
		return
	}
	schedule, err = s.store.SaveBackupSchedule(r.Context(), schedule, time.Now().UTC())
	writeRuntimeResult(w, schedule, err)
}

func (s *Server) deleteBackupSchedule(w http.ResponseWriter, r *http.Request) {
	if !queryBool(r, "confirm") {
		writeRuntimeError(w, fmt.Errorf("%w: confirm=true is required; existing archives will not be deleted", datafiles.ErrInvalid))
		return
	}
	id, err := operationID(r)
	if err == nil {
		err = s.store.DeleteBackupSchedule(r.Context(), id)
	}
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) runBackupSchedule(w http.ResponseWriter, r *http.Request) {
	id, err := operationID(r)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	schedules, err := s.store.BackupSchedules(r.Context())
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	for _, schedule := range schedules {
		if schedule.ID != id {
			continue
		}
		run, err := s.startDataOperation(r.Context(), dataops.Request{
			Action: "volume-export", Resource: schedule.Resource, Identity: schedule.Resource.Fingerprint(),
			Directory: schedule.Directory, ScheduleID: schedule.ID, Retention: schedule.Retention, Trigger: "manual-backup", Confirm: true,
		})
		if err != nil {
			writeRuntimeError(w, err)
			return
		}
		writeJSONStatus(w, http.StatusAccepted, run)
		return
	}
	http.Error(w, "Backup schedule not found.", http.StatusNotFound)
}

func (s *Server) checkScheduledBackups(ctx context.Context) {
	s.dataMu.Lock()
	defer s.dataMu.Unlock()
	if s.runtimeContext == nil || ctx.Err() != nil || len(s.dataCancels) > 0 || len(s.dataUnstored) > 0 || s.hasRuntimeOperations() {
		return
	}
	if !s.beginRuntimeOperation() {
		return
	}
	run, err := s.store.ClaimBackup(ctx, time.Now().UTC())
	if err != nil || run == nil {
		s.endRuntimeOperation()
		if err != nil {
			log.Printf("claim local volume backup: %v", err)
		}
		return
	}
	s.launchDataOperationLocked(*run)
}

func (s *Server) applyBackupRetention(ctx context.Context, run dataops.Operation, newest dataops.Archive) error {
	if run.Request.Retention < 1 {
		return errors.New("invalid backup retention; no archives were deleted")
	}
	verified, err := portodocker.InspectVolumeArchive(ctx, newest.Path)
	if err != nil || verified.SHA256 != newest.SHA256 {
		return errors.Join(errors.New("replacement backup failed revalidation; no recovery point was deleted"), err)
	}
	old, err := s.store.VerifiedBackupOperations(ctx, run.Request.ScheduleID)
	if err != nil {
		return err
	}
	kept := 1
	for _, previous := range old {
		archive := previous.Result.Archive
		if archive == nil || archive.Path == "" {
			continue
		}
		if kept < run.Request.Retention {
			kept++
			continue
		}
		if archive.Resource.Fingerprint() != newest.Resource.Fingerprint() || filepath.Dir(archive.Path) != filepath.Dir(newest.Path) {
			return errors.New("refusing retention outside the exact backup source and directory")
		}
		current, err := portodocker.InspectVolumeArchive(ctx, archive.Path)
		if err != nil || current.SHA256 != archive.SHA256 {
			return errors.Join(errors.New("older backup ownership or integrity changed; it was not deleted"), err)
		}
		root, err := os.OpenRoot(filepath.Dir(archive.Path))
		if err != nil {
			return err
		}
		removeErr := root.Remove(filepath.Base(archive.Path))
		if err := errors.Join(removeErr, root.Close()); err != nil {
			return err
		}
		if err := s.store.ExpireBackupArchive(ctx, previous.ID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) recordDockerStorageOperation(_ context.Context, request dataops.Request, result dataops.Result, operationErr error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	run, err := s.store.BeginDataOperation(ctx, request, time.Now().UTC())
	if err != nil {
		return err
	}
	status, message := "succeeded", ""
	if operationErr != nil {
		status, message = "failed", operationErr.Error()
	}
	return s.store.FinishDataOperation(ctx, run.ID, status, result, message, time.Now().UTC())
}

func (s *Server) migrationContexts(w http.ResponseWriter, r *http.Request) {
	value, err := s.docker.MigrationContexts(r.Context())
	writeRuntimeResult(w, value, err)
}

func (s *Server) migrationInventory(w http.ResponseWriter, r *http.Request) {
	value, err := s.docker.MigrationInventory(r.Context(), r.URL.Query().Get("context"))
	writeRuntimeResult(w, value, err)
}
