package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/dataops"
)

const sourceHelperOwnerLabel = "io.porto.migration.temporary"

type sourceAPIError struct {
	status  int
	message string
}

func (err *sourceAPIError) Error() string {
	return fmt.Sprintf("source runtime HTTP %d: %s", err.status, err.message)
}

func sourceNotFound(err error) bool {
	var apiErr *sourceAPIError
	return errors.As(err, &apiErr) && apiErr.status == http.StatusNotFound
}

func (source *migrationClient) findVolumeContainer(ctx context.Context, volume string) (string, string, error) {
	var containers []struct {
		ID string `json:"Id"`
	}
	if err := source.get(ctx, "/containers/json", url.Values{"all": {"true"}}, &containers); err != nil {
		return "", "", err
	}
	for _, candidate := range containers {
		var inspected migrationContainer
		if err := source.get(ctx, "/containers/"+url.PathEscape(candidate.ID)+"/json", nil, &inspected); err != nil {
			return "", "", err
		}
		for _, mounted := range inspected.Mounts {
			if mounted.Type == "volume" && mounted.Name == volume {
				return inspected.ID, mounted.Destination, nil
			}
		}
	}
	return "", "", nil
}

func (source *migrationClient) openVolumeArchive(
	ctx context.Context,
	request dataops.Request,
	volume, identity string,
	reserve, clear func(context.Context, dataops.SourceTemporary) error,
) (io.ReadCloser, func(context.Context) error, error) {
	container, mountedPath, err := source.findVolumeContainer(ctx, volume)
	if err != nil {
		return nil, nil, err
	}
	if container != "" {
		response, err := source.request(ctx, http.MethodGet, "/containers/"+url.PathEscape(container)+"/archive",
			url.Values{"path": {mountedPath + "/."}}, nil)
		if err != nil {
			return nil, nil, err
		}
		return response.Body, func(context.Context) error { return nil }, nil
	}
	if !request.AllowSourceHelper {
		return nil, nil, fmt.Errorf("%w: unattached volume %s requires explicit temporary source-helper consent; the helper remains stopped and its volume mount is read-only/no-copy", ErrUnsupported, volume)
	}
	if reserve == nil || clear == nil {
		return nil, nil, errors.New("source helper ownership ledger is unavailable; source was not modified")
	}
	var version struct {
		OS   string `json:"Os"`
		Arch string
	}
	if err := source.get(ctx, "/version", nil, &version); err != nil {
		return nil, nil, err
	}
	if version.OS != "linux" {
		return nil, nil, fmt.Errorf("%w: migration helpers require a Linux source engine", ErrUnsupported)
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return nil, nil, err
	}
	owner := hex.EncodeToString(random)
	archive, _, err := scratchMigrationImage(owner, version.Arch)
	if err != nil {
		return nil, nil, err
	}
	image := dataops.SourceTemporary{
		Context: source.context.Name, Endpoint: source.context.Endpoint, Kind: "image",
		Name: "porto-migration-helper:" + owner, Owner: owner,
	}
	containerLease := dataops.SourceTemporary{
		Context: source.context.Name, Endpoint: source.context.Endpoint, Kind: "container",
		Name: "porto-migration-" + owner, Owner: owner,
	}
	if err := reserve(ctx, image); err != nil {
		return nil, nil, err
	}
	cleanup := func(ctx context.Context) error {
		containerErr := source.cleanupTemporary(ctx, containerLease)
		if containerErr == nil {
			containerErr = clear(ctx, containerLease)
		}
		if containerErr != nil {
			return containerErr
		}
		imageErr := source.cleanupTemporary(ctx, image)
		if imageErr == nil {
			imageErr = clear(ctx, image)
		}
		return imageErr
	}
	fail := func(err error) (io.ReadCloser, func(context.Context) error, error) {
		cleanContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		return nil, nil, errors.Join(err, cleanup(cleanContext))
	}
	response, err := source.request(ctx, http.MethodPost, "/images/load", url.Values{"quiet": {"true"}}, bytes.NewReader(archive))
	if err != nil {
		return fail(err)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1024*1024))
	for {
		var status struct{ Error string }
		decodeErr := decoder.Decode(&status)
		if decodeErr == io.EOF {
			break
		}
		if decodeErr != nil || status.Error != "" {
			return fail(errors.Join(decodeErr, errors.New("source could not load its temporary empty helper image: "+status.Error), response.Body.Close()))
		}
	}
	if err := response.Body.Close(); err != nil {
		return fail(err)
	}
	image.ID, err = source.temporaryIdentity(ctx, image)
	if err != nil {
		return fail(err)
	}
	if err := reserve(ctx, image); err != nil {
		return fail(err)
	}
	if err := reserve(ctx, containerLease); err != nil {
		return fail(err)
	}
	create := map[string]any{
		"Image": image.ID, "Cmd": []string{"/porto-migration-never-started"},
		"Labels":          map[string]string{sourceHelperOwnerLabel: owner, migrationSourceLabel: identity},
		"NetworkDisabled": true,
		"HostConfig": map[string]any{
			"ReadonlyRootfs": true, "NetworkMode": "none", "AutoRemove": false,
			"Mounts": []map[string]any{{
				"Type": "volume", "Source": volume, "Target": "/data", "ReadOnly": true,
				"VolumeOptions": map[string]bool{"NoCopy": true},
			}},
			"RestartPolicy": map[string]string{"Name": "no"},
		},
	}
	document, err := json.Marshal(create)
	if err != nil {
		return fail(err)
	}
	response, err = source.request(ctx, http.MethodPost, "/containers/create", url.Values{"name": {containerLease.Name}}, bytes.NewReader(document))
	if err != nil {
		return fail(err)
	}
	var created struct {
		ID string `json:"Id"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(response.Body, 1024*1024)).Decode(&created)
	if err := errors.Join(decodeErr, response.Body.Close()); err != nil || created.ID == "" {
		return fail(errors.Join(errors.New("source helper create returned no identity"), err))
	}
	containerLease.ID = created.ID
	if err := reserve(ctx, containerLease); err != nil {
		return fail(err)
	}
	if err := source.verifyTemporary(ctx, containerLease); err != nil {
		return fail(err)
	}
	var current struct{ Name, CreatedAt, Mountpoint string }
	if err := source.get(ctx, "/volumes/"+url.PathEscape(volume), nil, &current); err != nil {
		return fail(err)
	}
	digest := sha256.Sum256([]byte(current.Name + "\x00" + current.CreatedAt + "\x00" + current.Mountpoint))
	if hex.EncodeToString(digest[:]) != identity {
		return fail(fmt.Errorf("%w: source volume identity changed while creating its readonly helper", datafiles.ErrConflict))
	}
	response, err = source.request(ctx, http.MethodGet, "/containers/"+url.PathEscape(containerLease.ID)+"/archive", url.Values{"path": {"/data/."}}, nil)
	if err != nil {
		return fail(err)
	}
	return response.Body, cleanup, nil
}

func scratchMigrationImage(owner, architecture string) ([]byte, string, error) {
	if !regexp.MustCompile(`^[a-z0-9]+$`).MatchString(architecture) {
		return nil, "", datafiles.ErrInvalid
	}
	config := map[string]any{
		"architecture": architecture, "os": "linux",
		"config": map[string]any{"Labels": map[string]string{sourceHelperOwnerLabel: owner}},
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{}},
	}
	document, err := json.Marshal(config)
	if err != nil {
		return nil, "", err
	}
	hash := sha256.Sum256(document)
	digest := hex.EncodeToString(hash[:])
	configName := digest + ".json"
	manifest, err := json.Marshal([]map[string]any{{
		"Config": configName, "RepoTags": []string{"porto-migration-helper:" + owner}, "Layers": []string{},
	}})
	if err != nil {
		return nil, "", err
	}
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for _, file := range []struct {
		name string
		data []byte
	}{{configName, document}, {"manifest.json", manifest}} {
		if err := writer.WriteHeader(&tar.Header{Name: file.name, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(file.data))}); err != nil {
			return nil, "", err
		}
		if _, err := writer.Write(file.data); err != nil {
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return archive.Bytes(), "sha256:" + digest, nil
}

func (source *migrationClient) verifyTemporary(ctx context.Context, resource dataops.SourceTemporary) error {
	_, err := source.temporaryIdentity(ctx, resource)
	return err
}

func (source *migrationClient) temporaryIdentity(ctx context.Context, resource dataops.SourceTemporary) (string, error) {
	id := resource.ID
	if id == "" {
		id = resource.Name
	}
	if resource.Context != source.context.Name || resource.Endpoint != source.context.Endpoint {
		return "", datafiles.ErrConflict
	}
	if resource.Kind == "container" {
		var inspected struct {
			ID     string `json:"Id"`
			Name   string
			Config struct{ Labels map[string]string }
		}
		if err := source.get(ctx, "/containers/"+url.PathEscape(id)+"/json", nil, &inspected); err != nil {
			return "", err
		}
		if inspected.Config.Labels[sourceHelperOwnerLabel] != resource.Owner ||
			strings.TrimPrefix(inspected.Name, "/") != resource.Name ||
			resource.ID != "" && inspected.ID != resource.ID {
			return "", fmt.Errorf("%w: source helper container ownership changed; it was not deleted", datafiles.ErrConflict)
		}
		return inspected.ID, nil
	}
	var inspected struct {
		ID       string `json:"Id"`
		RepoTags []string
		Config   struct{ Labels map[string]string }
	}
	if err := source.get(ctx, "/images/"+url.PathEscape(resource.Name)+"/json", nil, &inspected); err != nil {
		return "", err
	}
	if inspected.ID == "" || resource.ID != "" && inspected.ID != resource.ID ||
		inspected.Config.Labels[sourceHelperOwnerLabel] != resource.Owner || len(inspected.RepoTags) != 1 || inspected.RepoTags[0] != resource.Name {
		return "", fmt.Errorf("%w: source helper image ownership/reference changed; it was not deleted", datafiles.ErrConflict)
	}
	return inspected.ID, nil
}

func (source *migrationClient) cleanupTemporary(ctx context.Context, resource dataops.SourceTemporary) error {
	id, err := source.temporaryIdentity(ctx, resource)
	if err != nil {
		if sourceNotFound(err) {
			return nil
		}
		return err
	}
	var response *http.Response
	if resource.Kind == "container" {
		response, err = source.request(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id), url.Values{"force": {"true"}, "v": {"false"}}, nil)
	} else {
		response, err = source.request(ctx, http.MethodDelete, "/images/"+url.PathEscape(id), url.Values{"force": {"false"}, "noprune": {"true"}}, nil)
	}
	if sourceNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return response.Body.Close()
}

func (m *Manager) SetMigrationTemporaryLedger(
	reserve, clear func(context.Context, dataops.SourceTemporary) error,
	pending func(context.Context) ([]dataops.SourceTemporary, error),
) {
	m.migrationTemporaryReserve, m.migrationTemporaryClear, m.migrationTemporaryPending = reserve, clear, pending
}

func (m *Manager) RecoverMigrationHelpers(ctx context.Context) error {
	if m.migrationTemporaryPending == nil {
		return nil
	}
	resources, err := m.migrationTemporaryPending(ctx)
	if err != nil {
		return err
	}
	var result error
	for _, resource := range resources {
		source, err := m.migrationSource(ctx, resource.Context)
		if err == nil && source.context.Endpoint != resource.Endpoint {
			err = datafiles.ErrConflict
		}
		if err == nil {
			err = source.cleanupTemporary(ctx, resource)
		}
		if err == nil {
			err = m.migrationTemporaryClear(ctx, resource)
		}
		result = errors.Join(result, err)
	}
	return result
}
