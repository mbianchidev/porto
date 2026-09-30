package runtimefiles

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/mbianchidev/porto/internal/datafiles"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

type Descriptor struct {
	Resource     datafiles.Resource `json:"resource"`
	Address      string             `json:"address,omitempty"`
	Namespace    string             `json:"namespace,omitempty"`
	RootPath     string             `json:"rootPath,omitempty"`
	Snapshotter  string             `json:"snapshotter,omitempty"`
	SnapshotKey  string             `json:"snapshotKey,omitempty"`
	PID          uint32             `json:"pid,omitempty"`
	Mounts       []specs.Mount      `json:"mounts,omitempty"`
	Display      []datafiles.Mount  `json:"display,omitempty"`
	Owner        string             `json:"owner,omitempty"`
	Platform     string             `json:"platform,omitempty"`
	UIDMap       []IDMap            `json:"uidMap,omitempty"`
	GIDMap       []IDMap            `json:"gidMap,omitempty"`
	RuntimePID   int32              `json:"runtimePid,omitempty"`
	NamespacePID int32              `json:"namespacePid,omitempty"`
}

type IDMap struct {
	Namespace int64 `json:"namespace"`
	Host      int64 `json:"host"`
	Size      int64 `json:"size"`
}

type Envelope struct {
	Descriptor Descriptor        `json:"descriptor"`
	Request    datafiles.Request `json:"request"`
}

type Attachment struct {
	Token        string             `json:"token"`
	Path         string             `json:"path"`
	Resource     datafiles.Resource `json:"resource"`
	Identity     string             `json:"identity"`
	ReadOnly     bool               `json:"readOnly"`
	Snapshotter  string             `json:"snapshotter,omitempty"`
	SnapshotKey  string             `json:"snapshotKey,omitempty"`
	NamespacePID int32              `json:"namespacePid,omitempty"`
}

func Run(ctx context.Context, input io.Reader, output io.Writer) error {
	reader := bufio.NewReaderSize(input, 1024*1024)
	header, err := reader.ReadSlice('\n')
	if err != nil {
		return fmt.Errorf("read runtime filesystem request: %w", err)
	}
	var envelope Envelope
	decoder := json.NewDecoder(bytes.NewReader(header))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return fmt.Errorf("decode runtime filesystem request: %w", err)
	}
	if envelope.Descriptor.Resource.ID == "" || envelope.Request.Identity != envelope.Descriptor.Resource.Fingerprint() {
		return datafiles.ErrConflict
	}
	descriptor, err := resolveOwnership(envelope.Descriptor)
	if err != nil {
		return err
	}
	if handled, err := dispatchRuntimeNamespace(ctx, descriptor, envelope.Request, reader, output); handled {
		return err
	}
	switch envelope.Request.Action {
	case "attach":
		attachment, err := attach(ctx, descriptor, envelope.Request.Writable)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(attachment)
	case "detach":
		if err := detach(ctx, descriptor, envelope.Request.Token); err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(map[string]bool{"detached": true})
	default:
		readOnly := envelope.Request.Action != "write" && envelope.Request.Action != "delete"
		return withDirectory(ctx, descriptor, readOnly, func(descriptor Descriptor) error {
			return ExecuteDirectory(ctx, descriptor, envelope.Request, reader, output)
		})
	}
}

func ExecuteDirectory(ctx context.Context, descriptor Descriptor, request datafiles.Request, input io.Reader, output io.Writer) error {
	if request.Identity != descriptor.Resource.Fingerprint() {
		return datafiles.ErrConflict
	}
	if descriptor.Resource.Kind == "image" {
		descriptor.Resource.ReadOnly = true
	}
	if request.Path == "" {
		request.Path = "."
	}
	if err := datafiles.ValidatePath(request.Path); err != nil {
		return err
	}
	readOnly := readOnlyPath(descriptor, request.Path)
	if (request.Action == "write" || request.Action == "delete" || request.Action == "restore" || request.Action == "empty") && readOnly {
		return fmt.Errorf("%w: this filesystem or mount is read-only", datafiles.ErrUnsupported)
	}
	var result any
	var err error
	switch request.Action {
	case "list":
		var listing datafiles.Listing
		listing, err = datafiles.List(ctx, descriptor.RootPath, request.Path, descriptor.Resource)
		listing.ReadOnly, listing.Mounts = readOnly, descriptor.Display
		for index := range listing.Entries {
			entry := &listing.Entries[index]
			entry.UID, entry.GID, err = descriptor.namespaceOwner(entry.UID, entry.GID)
			if err != nil {
				return err
			}
		}
		if descriptor.Resource.Kind == "container" {
			listing.Message = "Container writable layer; mounted volumes and bind mounts are listed separately. Absolute or escaping symlinks are not traversed."
		}
		result = listing
	case "read":
		result, err = datafiles.Read(ctx, descriptor.RootPath, request.Path, readOnly)
	case "download":
		return datafiles.Download(ctx, descriptor.RootPath, request.Path, output, datafiles.MaxUploadBytes)
	case "write":
		content, readErr := io.ReadAll(io.LimitReader(input, datafiles.MaxUploadBytes+1))
		if readErr != nil {
			return readErr
		}
		uid, gid, ownerErr := descriptor.hostOwner(0, 0)
		if ownerErr != nil {
			return ownerErr
		}
		err = datafiles.WriteOwned(ctx, descriptor.RootPath, request.Path, content, request.SHA256, uid, gid)
		result = map[string]bool{"written": err == nil}
	case "delete":
		if !request.Confirm {
			return fmt.Errorf("%w: explicit deletion confirmation is required", datafiles.ErrInvalid)
		}
		err = datafiles.Delete(ctx, descriptor.RootPath, request.Path, request.SHA256)
		result = map[string]bool{"deleted": err == nil}
	case "export":
		_, err := datafiles.ExportWithOwners(ctx, output, descriptor.RootPath, descriptor.Resource, descriptor.namespaceOwner)
		return err
	case "manifest":
		result, err = datafiles.ExportWithOwners(ctx, io.Discard, descriptor.RootPath, descriptor.Resource, descriptor.namespaceOwner)
	case "usage":
		logical, allocated, usageErr := datafiles.DiskUsage(ctx, descriptor.RootPath)
		result, err = map[string]any{"logicalBytes": logical, "allocatedBytes": allocated}, usageErr
	case "restore", "empty":
		if descriptor.Resource.Kind != "volume" || !request.Confirm {
			return fmt.Errorf("%w: only a confirmed, idle volume can be restored or emptied", datafiles.ErrInvalid)
		}
		err = restoreVolume(ctx, descriptor, request, input)
		result = map[string]bool{"committed": err == nil}
	default:
		return fmt.Errorf("%w: unknown runtime file action", datafiles.ErrInvalid)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(result)
}

func readOnlyPath(descriptor Descriptor, relative string) bool {
	if descriptor.Resource.ReadOnly || descriptor.Resource.Kind == "image" {
		return true
	}
	absolute := "/" + strings.TrimPrefix(relative, "./")
	for _, mounted := range descriptor.Display {
		if mounted.ReadOnly && (absolute == mounted.Path || strings.HasPrefix(absolute, strings.TrimSuffix(mounted.Path, "/")+"/")) {
			return true
		}
	}
	return false
}

func restoreVolume(ctx context.Context, descriptor Descriptor, request datafiles.Request, input io.Reader) (err error) {
	if err := ensureVolumeIdle(ctx, descriptor); err != nil {
		return err
	}
	if request.Action == "empty" {
		if request.SHA256 == "" {
			return fmt.Errorf("%w: volume empty requires a content preview", datafiles.ErrInvalid)
		}
		return datafiles.ReplaceDirectory(ctx, descriptor.RootPath, func(string) error { return nil },
			func() error {
				if err := ensureVolumeIdle(ctx, descriptor); err != nil {
					return err
				}
				manifest, err := datafiles.ExportWithOwners(ctx, io.Discard, descriptor.RootPath, descriptor.Resource, descriptor.namespaceOwner)
				if err != nil {
					return err
				}
				digest, err := datafiles.ContentDigest(manifest)
				if err != nil || digest != request.SHA256 {
					return errors.Join(datafiles.ErrConflict, err)
				}
				return nil
			})
	}
	file, err := os.CreateTemp(path.Dir(descriptor.RootPath), ".porto-archive-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close(), os.Remove(file.Name())) }()
	limited := io.LimitReader(input, datafiles.MaxArchiveBytes+32*1024*1024+1)
	written, err := io.Copy(file, limited)
	if err != nil {
		return err
	}
	if written > datafiles.MaxArchiveBytes+32*1024*1024 {
		return datafiles.ErrLimit
	}
	return datafiles.ReplaceDirectory(ctx, descriptor.RootPath, func(stage string) error {
		return datafiles.Restore(ctx, file, stage, datafiles.RestoreOptions{
			PreserveOwnership: true, AvailableBytes: datafiles.AvailableSpace,
			MapOwner: descriptor.hostOwner,
		})
	}, func() error { return ensureVolumeIdle(ctx, descriptor) })
}
