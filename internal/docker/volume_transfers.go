package docker

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/dataops"
	"github.com/mbianchidev/porto/internal/runtimefiles"
)

const transferOwnerLabel = "io.porto.transfer.owner"

var volumeNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

type VolumePreview struct {
	Request      dataops.Request    `json:"request"`
	Resource     datafiles.Resource `json:"resource"`
	Owners       []StorageOwner     `json:"owners"`
	Destination  string             `json:"destination"`
	Archive      *dataops.Archive   `json:"archive,omitempty"`
	Files        []datafiles.Entry  `json:"files,omitempty"`
	Token        string             `json:"token"`
	Consistency  string             `json:"consistency"`
	Consequences string             `json:"consequences"`
}

func (m *Manager) PreviewVolume(ctx context.Context, request dataops.Request) (preview VolumePreview, err error) {
	preview = VolumePreview{
		Request: request, Owners: make([]StorageOwner, 0), Destination: request.Destination,
		Consistency:  "crash-consistent",
		Consequences: "Running writers are not stopped or quiesced. A filesystem copy is not an application-consistent database backup. Imports restore into staging and publish only after full validation.",
	}
	if !slices.Contains([]string{"volume-export", "volume-clone", "volume-import", "volume-restore", "volume-empty", "volume-remove"}, request.Action) {
		return preview, fmt.Errorf("%w: invalid volume transfer action", datafiles.ErrInvalid)
	}
	var descriptor runtimefiles.Descriptor
	if request.Action != "volume-import" {
		descriptor, err = m.FileDescriptor(ctx, "volume", request.Resource.Name)
		if err != nil {
			return preview, err
		}
		if request.Identity != "" && request.Identity != descriptor.Resource.Fingerprint() {
			return preview, datafiles.ErrConflict
		}
		preview.Resource = descriptor.Resource
		preview.Request.Resource, preview.Request.Identity = descriptor.Resource, descriptor.Resource.Fingerprint()
		preview.Owners, err = m.volumeOwners(ctx, descriptor)
		if err != nil {
			return preview, err
		}
		if request.Action == "volume-empty" || request.Action == "volume-restore" || request.Action == "volume-remove" {
			if err := m.validateVolumeMutation(ctx, descriptor, request.Action == "volume-remove"); err != nil {
				return preview, err
			}
		}
	}
	if request.Action == "volume-remove" {
		preview.Consequences = "Remove local volume " + descriptor.Resource.Name + " and all of its data permanently. Every container mount must be detached first. Other volumes and container definitions are unchanged."
		preview.Destination = "permanently removed"
	}
	if request.Action == "volume-clone" || request.Action == "volume-import" {
		if !volumeNamePattern.MatchString(request.Destination) {
			return preview, fmt.Errorf("%w: destination must be a new local volume name (1-128 letters, numbers, dots, underscores or hyphens)", datafiles.ErrInvalid)
		}
		volumes, err := m.Volumes(ctx)
		if err != nil {
			return preview, err
		}
		for _, volume := range volumes {
			if volume.Name == request.Destination {
				return preview, fmt.Errorf("%w: destination volume already exists; choose a new name or an explicit restore", datafiles.ErrConflict)
			}
		}
	}
	if request.Action == "volume-import" || request.Action == "volume-restore" {
		archive, err := InspectVolumeArchive(ctx, request.Archive)
		if err != nil {
			return preview, err
		}
		preview.Archive = &archive
		if request.Action == "volume-restore" {
			for _, owner := range preview.Owners {
				if containerdStateActive(owner.State) {
					return preview, fmt.Errorf("%w: stop volume writer %s before restore", ErrConflict, owner.Name)
				}
			}
			preview.Destination = descriptor.Resource.Name
			preview.Consequences += " The existing destination data will be replaced after validation; its original directory remains intact until the atomic publish."
		}
	}
	if request.Action == "volume-empty" {
		for _, owner := range preview.Owners {
			if containerdStateActive(owner.State) {
				return preview, fmt.Errorf("%w: stop volume writer %s before emptying it", ErrConflict, owner.Name)
			}
		}
		var output bytes.Buffer
		if err := m.RunFileRequest(ctx, descriptor, datafiles.Request{Action: "manifest", Identity: descriptor.Resource.Fingerprint()}, nil, &output); err != nil {
			return preview, err
		}
		var manifest datafiles.Manifest
		if err := json.Unmarshal(output.Bytes(), &manifest); err != nil {
			return preview, err
		}
		preview.Files = manifest.Entries
		preview.Consequences = "Empty removes exactly the previewed data from this idle volume after identity and content revalidation. Container definitions and other volumes are unchanged."
		preview.Request.Archive, err = datafiles.ContentDigest(manifest)
		if err != nil {
			return preview, err
		}
	}
	signature, err := json.Marshal(struct {
		Action, Identity, Destination, Archive string
		Owners                                 []StorageOwner
	}{request.Action, preview.Request.Identity, preview.Destination, archiveSignature(preview.Archive, preview.Request.Archive), preview.Owners})
	if err != nil {
		return preview, err
	}
	hash := sha256.Sum256(signature)
	preview.Token = hex.EncodeToString(hash[:])
	preview.Request.Preview = preview.Token
	return preview, nil
}

func archiveSignature(archive *dataops.Archive, fallback string) string {
	if archive != nil {
		return archive.SHA256
	}
	return fallback
}

func InspectVolumeArchive(ctx context.Context, archivePath string) (result dataops.Archive, err error) {
	root, file, err := datafiles.OpenManagedArchive(archivePath)
	if err != nil {
		return result, err
	}
	defer root.Close()
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > datafiles.MaxArchiveBytes+32*1024*1024 {
		return result, errors.Join(datafiles.ErrLimit, err)
	}
	manifest, err := datafiles.Validate(ctx, file)
	if err != nil || manifest.Resource.Kind != "volume" {
		return result, errors.Join(fmt.Errorf("%w: a versioned Porto volume archive is required", datafiles.ErrInvalid), err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return result, err
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, contextBoundReader{ctx, file}); err != nil {
		return result, err
	}
	return dataops.Archive{
		Path: archivePath, SHA256: hex.EncodeToString(hash.Sum(nil)), Manifest: manifest.SHA256,
		Bytes: info.Size(), Resource: manifest.Resource, Consistency: manifest.Consistency,
	}, nil
}

func (m *Manager) ExportVolume(ctx context.Context, resource datafiles.Resource, identity, destination string, progress func(string, int64) error) (result dataops.Archive, err error) {
	ctx, release, err := m.BeginDataTransaction(ctx)
	if err != nil {
		return result, err
	}
	defer release()
	descriptor, err := m.FileDescriptor(ctx, "volume", resource.Name)
	if err != nil {
		return result, err
	}
	if identity != descriptor.Resource.Fingerprint() {
		return result, datafiles.ErrConflict
	}
	base, relative, err := datafiles.ManagedLocation(destination)
	if err != nil {
		return result, err
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return result, err
	}
	defer root.Close()
	parent := filepath.Dir(relative)
	if err := root.MkdirAll(parent, 0o700); err != nil {
		return result, err
	}
	if _, err := root.Lstat(relative); err == nil {
		return result, fmt.Errorf("%w: export destination already exists", datafiles.ErrConflict)
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	var measured bytes.Buffer
	if err := m.RunFileRequest(ctx, descriptor, datafiles.Request{Action: "usage", Identity: identity}, nil, &measured); err != nil {
		return result, err
	}
	var usage struct {
		LogicalBytes int64 `json:"logicalBytes"`
	}
	if err := json.Unmarshal(measured.Bytes(), &usage); err != nil {
		return result, err
	}
	available, err := datafiles.AvailableSpace(filepath.Join(base, parent))
	if err != nil || usage.LogicalBytes < 0 || uint64(usage.LogicalBytes)+64*1024*1024 > available {
		return result, errors.Join(fmt.Errorf("%w: insufficient local space for export", datafiles.ErrLimit), err)
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return result, err
	}
	temporary := filepath.Join(parent, ".porto-export-"+hex.EncodeToString(random))
	file, err := root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, file.Close(), root.Remove(temporary)) }()
	counter := &transferWriter{ctx: ctx, output: file, progress: progress, phase: "Exporting crash-consistent volume data"}
	err = m.RunFileRequest(ctx, descriptor, datafiles.Request{Action: "export", Identity: identity}, nil, counter)
	if err != nil {
		return result, err
	}
	if err := file.Sync(); err != nil {
		return result, err
	}
	result, err = InspectVolumeArchive(ctx, filepath.Join(base, temporary))
	if err != nil {
		return result, err
	}
	if result.Resource.Fingerprint() != identity {
		return result, datafiles.ErrConflict
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := root.Link(temporary, relative); err != nil {
		return result, fmt.Errorf("publish verified archive without overwrite: %w", err)
	}
	result.Path = destination
	return result, progress("Verified archive published", result.Bytes)
}

func (m *Manager) RestoreVolume(ctx context.Context, resource datafiles.Resource, identity, archivePath, archiveSHA string, progress func(string, int64) error) (err error) {
	descriptor, err := m.FileDescriptor(ctx, "volume", resource.Name)
	if err != nil {
		return err
	}
	if descriptor.Resource.Fingerprint() != identity {
		return datafiles.ErrConflict
	}
	archive, err := InspectVolumeArchive(ctx, archivePath)
	if err != nil {
		return err
	}
	if archive.SHA256 != archiveSHA {
		return fmt.Errorf("%w: archive changed after preview", datafiles.ErrConflict)
	}
	root, file, err := datafiles.OpenManagedArchive(archivePath)
	if err != nil {
		return err
	}
	defer root.Close()
	defer file.Close()
	if err := progress("Restoring into isolated staging; original data unchanged", 0); err != nil {
		return err
	}
	var output bytes.Buffer
	return m.RunFileRequest(ctx, descriptor, datafiles.Request{
		Action: "restore", Identity: identity, Confirm: true, SHA256: archiveSHA,
	}, &transferReader{ctx: ctx, input: file, progress: progress}, &output)
}

func (m *Manager) CloneVolume(ctx context.Context, source datafiles.Resource, identity, destination, temporaryArchive string, progress func(string, int64) error) (result dataops.Result, err error) {
	ctx, release, err := m.BeginDataTransaction(ctx)
	if err != nil {
		return result, err
	}
	defer release()
	archive, err := m.ExportVolume(ctx, source, identity, temporaryArchive, progress)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, os.Remove(temporaryArchive)) }()
	return m.ImportVolume(ctx, destination, archive.Path, archive.SHA256, progress)
}

func (m *Manager) ImportVolume(ctx context.Context, destination, archivePath, archiveSHA string, progress func(string, int64) error) (dataops.Result, error) {
	return m.importVolume(ctx, destination, archivePath, archiveSHA, progress, nil, nil)
}

func (m *Manager) importVolume(ctx context.Context, destination, archivePath, archiveSHA string, progress func(string, int64) error, labels map[string]string, publish func(datafiles.Resource) error) (result dataops.Result, err error) {
	ctx, release, err := m.BeginDataTransaction(ctx)
	if err != nil {
		return result, err
	}
	defer release()
	if !volumeNamePattern.MatchString(destination) {
		return result, datafiles.ErrInvalid
	}
	volumes, err := m.Volumes(ctx)
	if err != nil {
		return result, err
	}
	for _, volume := range volumes {
		if volume.Name == destination {
			return result, fmt.Errorf("%w: destination volume already exists", datafiles.ErrConflict)
		}
	}
	archive, err := InspectVolumeArchive(ctx, archivePath)
	if err != nil {
		return result, err
	}
	if archive.SHA256 != archiveSHA {
		return result, fmt.Errorf("%w: archive changed before creating the destination", datafiles.ErrConflict)
	}
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return result, err
	}
	owner := hex.EncodeToString(token)
	temporary := dataops.VolumeTemporary{Owner: owner, Destination: destination}
	if m.volumeTemporaryReserve != nil {
		if err := m.volumeTemporaryReserve(ctx, temporary); err != nil {
			return result, fmt.Errorf("persist temporary volume ownership before creation: %w", err)
		}
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		err = errors.Join(err, m.cleanupVolumeTemporary(cleanupContext, temporary))
	}()
	labels = cloneStringMap(labels)
	if labels == nil {
		labels = make(map[string]string)
	}
	labels[transferOwnerLabel] = owner
	created, createErr := m.CreateVolume(ctx, destination, "local", labels)
	if createErr != nil {
		return result, createErr
	}
	descriptor, err := m.FileDescriptor(ctx, "volume", created.Name)
	if err != nil {
		return result, err
	}
	if err := m.RestoreVolume(ctx, descriptor.Resource, descriptor.Resource.Fingerprint(), archivePath, archiveSHA, progress); err != nil {
		return result, err
	}
	if publish != nil {
		if err := publish(descriptor.Resource); err != nil {
			return result, fmt.Errorf("record verified volume migration before publication: %w", err)
		}
	}
	if m.volumeTemporaryClear != nil {
		if err := m.volumeTemporaryClear(ctx, temporary); err != nil {
			return result, fmt.Errorf("persist verified volume publication: %w", err)
		}
	}
	committed = true
	result.Steps = []dataops.Step{{Kind: "volume", Source: archivePath, Destination: destination, ID: descriptor.Resource.ID, Status: "succeeded"}}
	result.Message = "Verified archive restored into a new local volume; the source is unchanged."
	return result, nil
}

func (m *Manager) SetVolumeTransferLedger(
	reserve func(context.Context, dataops.VolumeTemporary) error,
	clear func(context.Context, dataops.VolumeTemporary) error,
	pending func(context.Context) ([]dataops.VolumeTemporary, error),
) {
	m.volumeTemporaryReserve, m.volumeTemporaryClear, m.volumeTemporaryPending = reserve, clear, pending
}

func (m *Manager) cleanupVolumeTemporary(ctx context.Context, temporary dataops.VolumeTemporary) error {
	if !volumeNamePattern.MatchString(temporary.Destination) {
		return errors.New("temporary volume reservation has an invalid destination; nothing was deleted")
	}
	owner, err := hex.DecodeString(temporary.Owner)
	if err != nil || len(owner) != 16 {
		return errors.New("temporary volume reservation has invalid ownership; nothing was deleted")
	}
	document, inspectErr := m.InspectVolume(ctx, temporary.Destination)
	if !missingDockerObject(inspectErr, "volume") {
		var volume struct{ Labels map[string]string }
		if inspectErr == nil {
			inspectErr = json.Unmarshal(document, &volume)
		}
		if inspectErr != nil || volume.Labels[transferOwnerLabel] != temporary.Owner {
			return errors.Join(errors.New("temporary volume ownership could not be proven; no volume was deleted"), inspectErr)
		}
		if err := m.removeVolume(ctx, temporary.Destination, false, document); err != nil {
			return err
		}
	}
	if m.volumeTemporaryClear != nil {
		return m.volumeTemporaryClear(ctx, temporary)
	}
	return nil
}

func (m *Manager) RecoverVolumeTransfers(ctx context.Context) error {
	if m.volumeTemporaryPending == nil {
		return nil
	}
	ctx, release, err := m.BeginDataTransaction(ctx)
	if err != nil {
		return err
	}
	defer release()
	pending, err := m.volumeTemporaryPending(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, temporary := range pending {
		if err := m.cleanupVolumeTemporary(ctx, temporary); err != nil {
			failures = append(failures, fmt.Errorf("recover interrupted volume %s: %w", temporary.Destination, err))
		}
	}
	return errors.Join(failures...)
}

type transferWriter struct {
	ctx      context.Context
	output   io.Writer
	progress func(string, int64) error
	phase    string
	bytes    int64
	last     time.Time
}

func (w *transferWriter) Write(buffer []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if w.bytes+int64(len(buffer)) > datafiles.MaxArchiveBytes+32*1024*1024 {
		return 0, datafiles.ErrLimit
	}
	n, err := w.output.Write(buffer)
	w.bytes += int64(n)
	if err == nil && time.Since(w.last) > 500*time.Millisecond {
		err = w.progress(w.phase, w.bytes)
		w.last = time.Now()
	}
	return n, err
}

type transferReader struct {
	ctx      context.Context
	input    io.Reader
	progress func(string, int64) error
	bytes    int64
	last     time.Time
}

func (r *transferReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.input.Read(buffer)
	r.bytes += int64(n)
	if err == nil && time.Since(r.last) > 500*time.Millisecond {
		err = r.progress("Transferring verified archive to staging", r.bytes)
		r.last = time.Now()
	}
	return n, err
}

type contextBoundReader struct {
	ctx   context.Context
	input io.Reader
}

func (r contextBoundReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.input.Read(buffer[:min(len(buffer), 64*1024)])
}
