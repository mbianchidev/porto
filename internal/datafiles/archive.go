package datafiles

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"reflect"
	"slices"
	"strings"
	"time"
)

const manifestName = "porto-manifest.json"

func Export(ctx context.Context, output io.Writer, directory string, resource Resource) (manifest Manifest, err error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return manifest, fmt.Errorf("open export source: %w", err)
	}
	defer root.Close()
	writer := tar.NewWriter(output)
	defer func() { err = errors.Join(err, writer.Close()) }()
	manifest = Manifest{
		Version: 1, Resource: resource, CreatedAt: time.Now().UTC(),
		Consistency: "crash-consistent", Entries: make([]Entry, 0),
	}
	links := make(map[string]string)
	err = fs.WalkDir(root.FS(), ".", func(name string, item fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		if len(manifest.Entries) >= MaxEntries {
			return ErrLimit
		}
		if err := ValidatePath(name); err != nil {
			return err
		}
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		entry, err := entryFromInfo(name, info)
		if err != nil {
			return err
		}
		if entry.Type == "symlink" {
			entry.LinkTarget, err = root.Readlink(name)
			if err != nil {
				return err
			}
			if err := validateLink(name, entry.LinkTarget, false); err != nil {
				return err
			}
		}
		if entry.Type == "file" {
			if key := fileIdentity(info); key != "" {
				if original, exists := links[key]; exists {
					entry.Type, entry.LinkTarget, entry.Size = "hardlink", original, 0
				} else {
					links[key] = name
				}
			}
		}
		if entry.Size > MaxArchiveBytes-manifest.Bytes {
			return ErrLimit
		}
		header := headerForEntry(entry)
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if entry.Type == "file" {
			file, err := root.Open(name)
			if err != nil {
				return err
			}
			opened, statErr := file.Stat()
			if statErr != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
				return errors.Join(ErrConflict, statErr, file.Close())
			}
			hash := sha256.New()
			_, copyErr := io.CopyN(io.MultiWriter(writer, hash), contextReader{file, ctx.Err}, entry.Size)
			after, afterErr := file.Stat()
			closeErr := file.Close()
			if copyErr != nil || afterErr != nil || closeErr != nil {
				return errors.Join(copyErr, afterErr, closeErr)
			}
			if after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
				return fmt.Errorf("%w: %s was modified during export", ErrConflict, name)
			}
			entry.SHA256 = hex.EncodeToString(hash.Sum(nil))
			manifest.Bytes += entry.Size
		}
		manifest.Entries = append(manifest.Entries, entry)
		return nil
	})
	if err != nil {
		return manifest, fmt.Errorf("export filesystem: %w", err)
	}
	err = writeManifest(writer, &manifest)
	return manifest, err
}

// SealTar validates an ordinary Docker archive while adding the same integrity
// manifest used by local exports. It never extracts source data on the host.
func SealTar(ctx context.Context, input io.Reader, output io.Writer, resource Resource) (manifest Manifest, err error) {
	reader := tar.NewReader(contextReader{input, ctx.Err})
	writer := tar.NewWriter(output)
	defer func() { err = errors.Join(err, writer.Close()) }()
	manifest = Manifest{
		Version: 1, Resource: resource, CreatedAt: time.Now().UTC(),
		Consistency: "crash-consistent", Entries: make([]Entry, 0),
	}
	seen := make(map[string]Entry)
	for {
		header, readErr := reader.Next()
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return manifest, readErr
		}
		name := strings.TrimPrefix(header.Name, "./")
		name = strings.TrimSuffix(name, "/")
		if name == "." || name == "" {
			if header.Typeflag != tar.TypeDir {
				return manifest, fmt.Errorf("%w: invalid archive root", ErrInvalid)
			}
			continue
		}
		header.Name = "data/" + name
		if header.Typeflag == tar.TypeLink {
			header.Linkname = "data/" + strings.TrimPrefix(header.Linkname, "./")
		}
		entry, err := readArchiveEntry(header, seen)
		if err != nil {
			return manifest, err
		}
		if len(seen) >= MaxEntries || entry.Size > MaxArchiveBytes-manifest.Bytes {
			return manifest, ErrLimit
		}
		if err := writer.WriteHeader(headerForEntry(entry)); err != nil {
			return manifest, err
		}
		if entry.Type == "file" {
			hash := sha256.New()
			if _, err := io.CopyN(io.MultiWriter(writer, hash), reader, entry.Size); err != nil {
				return manifest, err
			}
			entry.SHA256 = hex.EncodeToString(hash.Sum(nil))
			manifest.Bytes += entry.Size
		}
		seen[entry.Path] = entry
		manifest.Entries = append(manifest.Entries, entry)
	}
	err = writeManifest(writer, &manifest)
	return manifest, err
}

func Validate(ctx context.Context, archive io.ReadSeeker) (Manifest, error) {
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return Manifest{}, err
	}
	reader := tar.NewReader(contextReader{archive, ctx.Err})
	seen := make(map[string]Entry)
	entries := make([]Entry, 0)
	var total int64
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return Manifest{}, fmt.Errorf("%w: integrity manifest is missing", ErrInvalid)
		}
		if err != nil {
			return Manifest{}, fmt.Errorf("read archive: %w", err)
		}
		if header.Name == manifestName {
			if header.Typeflag != tar.TypeReg || header.Size < 1 || header.Size > 32*1024*1024 {
				return Manifest{}, fmt.Errorf("%w: invalid integrity manifest", ErrInvalid)
			}
			var manifest Manifest
			decoder := json.NewDecoder(reader)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&manifest); err != nil {
				return manifest, fmt.Errorf("decode integrity manifest: %w", err)
			}
			if err := decoder.Decode(&struct{}{}); err != io.EOF {
				return manifest, fmt.Errorf("%w: extra manifest data", ErrInvalid)
			}
			if manifest.Version != 1 || manifest.Consistency != "crash-consistent" ||
				manifest.Resource.ID == "" || manifest.Bytes != total || !reflect.DeepEqual(manifest.Entries, entries) {
				return manifest, fmt.Errorf("%w: archive does not match its integrity manifest", ErrInvalid)
			}
			digest, err := manifestDigest(manifest)
			if err != nil || digest != manifest.SHA256 {
				return manifest, errors.Join(fmt.Errorf("%w: manifest checksum mismatch", ErrInvalid), err)
			}
			if _, err := reader.Next(); err != io.EOF {
				return manifest, fmt.Errorf("%w: archive contains data after the manifest", ErrInvalid)
			}
			return manifest, nil
		}
		entry, err := readArchiveEntry(header, seen)
		if err != nil {
			return Manifest{}, err
		}
		if len(seen) >= MaxEntries || entry.Size > MaxArchiveBytes-total {
			return Manifest{}, ErrLimit
		}
		if entry.Type == "file" {
			hash := sha256.New()
			if _, err := io.CopyN(hash, reader, entry.Size); err != nil {
				return Manifest{}, err
			}
			entry.SHA256 = hex.EncodeToString(hash.Sum(nil))
			total += entry.Size
		}
		seen[entry.Path] = entry
		entries = append(entries, entry)
	}
}

func Restore(ctx context.Context, archive io.ReadSeeker, destination string, options RestoreOptions) error {
	manifest, err := Validate(ctx, archive)
	if err != nil {
		return err
	}
	if options.AvailableBytes != nil {
		available, err := options.AvailableBytes(destination)
		if err != nil {
			return fmt.Errorf("check restore free space: %w", err)
		}
		if uint64(manifest.Bytes)+uint64(len(manifest.Entries))*4096+16*1024*1024 > available {
			return fmt.Errorf("%w: insufficient free space for a staged restore", ErrLimit)
		}
	}
	root, err := os.OpenRoot(destination)
	if err != nil {
		return err
	}
	defer root.Close()
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	items, readErr := directory.Readdirnames(1)
	closeErr := directory.Close()
	if len(items) > 0 {
		return fmt.Errorf("%w: restore requires an empty staging directory", ErrConflict)
	}
	if readErr != io.EOF || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	reader := tar.NewReader(contextReader{archive, ctx.Err})
	for _, entry := range manifest.Entries {
		header, err := reader.Next()
		if err != nil || header == nil || header.Name != "data/"+entry.Path {
			return errors.Join(fmt.Errorf("%w: archive changed after validation", ErrConflict), err)
		}
		if err := root.MkdirAll(path.Dir(entry.Path), 0o700); err != nil {
			return err
		}
		switch entry.Type {
		case "directory":
			err = root.MkdirAll(entry.Path, 0o700)
		case "file":
			file, openErr := root.OpenFile(entry.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if openErr != nil {
				return openErr
			}
			hash := sha256.New()
			_, copyErr := io.CopyN(io.MultiWriter(file, hash), reader, entry.Size)
			if copyErr == nil && hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
				copyErr = fmt.Errorf("%w: file checksum changed during restore", ErrConflict)
			}
			err = errors.Join(copyErr, file.Sync(), file.Close())
		case "hardlink":
			err = root.Link(entry.LinkTarget, entry.Path)
		case "symlink":
			err = root.Symlink(entry.LinkTarget, entry.Path)
		}
		if err != nil {
			return fmt.Errorf("restore %s: %w", entry.Path, err)
		}
		if options.PreserveOwnership {
			if err := root.Lchown(entry.Path, entry.UID, entry.GID); err != nil {
				return fmt.Errorf("restore ownership for %s: %w", entry.Path, err)
			}
		}
	}
	// Apply directory permissions last so restrictive source modes do not prevent
	// restoring their children. Never chmod a symbolic link.
	for _, entry := range slices.Backward(manifest.Entries) {
		if entry.Type == "symlink" {
			continue
		}
		modified, err := time.Parse(time.RFC3339Nano, entry.ModifiedAt)
		if err != nil {
			return err
		}
		if err := root.Chtimes(entry.Path, modified, modified); err != nil {
			return err
		}
		if err := root.Chmod(entry.Path, os.FileMode(entry.Mode)); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func ValidatePath(value string) error {
	if value == "" || len(value) > MaxPathBytes || strings.ContainsAny(value, "\\\x00\r\n") ||
		path.IsAbs(value) || path.Clean(value) != value || value == ".." || strings.HasPrefix(value, "../") {
		return fmt.Errorf("%w: path must be a normalized relative path inside the resource", ErrInvalid)
	}
	return nil
}

func validateLink(name, target string, hard bool) error {
	if target == "" || len(target) > MaxPathBytes || strings.ContainsAny(target, "\\\x00\r\n") || path.IsAbs(target) {
		return fmt.Errorf("%w: absolute or invalid link target at %s", ErrInvalid, name)
	}
	resolved := target
	if !hard {
		resolved = path.Clean(path.Join(path.Dir(name), target))
	}
	if err := ValidatePath(resolved); err != nil {
		return fmt.Errorf("%w: link escapes the resource at %s", err, name)
	}
	return nil
}

func readArchiveEntry(header *tar.Header, seen map[string]Entry) (Entry, error) {
	if !strings.HasPrefix(header.Name, "data/") || header.Size < 0 || header.Mode < 0 || header.Mode > 0o777 {
		return Entry{}, fmt.Errorf("%w: unsupported archive entry", ErrInvalid)
	}
	name := strings.TrimPrefix(header.Name, "data/")
	if err := ValidatePath(name); err != nil || name == "." {
		return Entry{}, errors.Join(fmt.Errorf("%w: invalid archive member", ErrInvalid), err)
	}
	if _, exists := seen[name]; exists {
		return Entry{}, fmt.Errorf("%w: duplicate archive member %s", ErrInvalid, name)
	}
	for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
		if existing, exists := seen[parent]; exists && existing.Type != "directory" {
			return Entry{}, fmt.Errorf("%w: archive member traverses a link or file", ErrInvalid)
		}
	}
	entry := Entry{
		Path: name, Size: header.Size, Mode: uint32(header.Mode), UID: header.Uid, GID: header.Gid,
		ModifiedAt: header.ModTime.UTC().Format(time.RFC3339Nano),
	}
	switch header.Typeflag {
	case tar.TypeReg, tar.TypeRegA:
		entry.Type = "file"
	case tar.TypeDir:
		entry.Type = "directory"
	case tar.TypeSymlink:
		entry.Type, entry.LinkTarget = "symlink", header.Linkname
	case tar.TypeLink:
		entry.Type = "hardlink"
		if !strings.HasPrefix(header.Linkname, "data/") {
			return entry, fmt.Errorf("%w: invalid hardlink", ErrInvalid)
		}
		entry.LinkTarget = strings.TrimPrefix(header.Linkname, "data/")
	default:
		return entry, fmt.Errorf("%w: devices, FIFOs and sockets cannot be transferred", ErrUnsupported)
	}
	if entry.Type != "file" && entry.Size != 0 {
		return entry, fmt.Errorf("%w: non-file archive entry has content", ErrInvalid)
	}
	if entry.Type == "symlink" || entry.Type == "hardlink" {
		if err := validateLink(name, entry.LinkTarget, entry.Type == "hardlink"); err != nil {
			return entry, err
		}
		if entry.Type == "hardlink" {
			target, exists := seen[entry.LinkTarget]
			if !exists || target.Type != "file" {
				return entry, fmt.Errorf("%w: hardlink must reference a preceding regular file", ErrInvalid)
			}
		}
	}
	return entry, nil
}

func headerForEntry(entry Entry) *tar.Header {
	modified, _ := time.Parse(time.RFC3339Nano, entry.ModifiedAt)
	header := &tar.Header{
		Name: "data/" + entry.Path, Mode: int64(entry.Mode), Uid: entry.UID, Gid: entry.GID,
		Size: entry.Size, ModTime: modified, Format: tar.FormatPAX, Linkname: entry.LinkTarget,
	}
	switch entry.Type {
	case "file":
		header.Typeflag = tar.TypeReg
	case "directory":
		header.Typeflag = tar.TypeDir
	case "symlink":
		header.Typeflag = tar.TypeSymlink
	case "hardlink":
		header.Typeflag, header.Linkname = tar.TypeLink, "data/"+entry.LinkTarget
	}
	return header
}

func writeManifest(writer *tar.Writer, manifest *Manifest) error {
	digest, err := manifestDigest(*manifest)
	if err != nil {
		return err
	}
	manifest.SHA256 = digest
	document, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if len(document) > 32*1024*1024 {
		return ErrLimit
	}
	if err := writer.WriteHeader(&tar.Header{
		Name: manifestName, Mode: 0o600, Size: int64(len(document)), Typeflag: tar.TypeReg,
	}); err != nil {
		return err
	}
	_, err = writer.Write(document)
	return err
}

func manifestDigest(manifest Manifest) (string, error) {
	manifest.SHA256 = ""
	document, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(document)
	return hex.EncodeToString(hash[:]), nil
}

func entryFromInfo(name string, info os.FileInfo) (Entry, error) {
	uid, gid, _ := fileMetadata(info)
	entry := Entry{
		Path: name, Mode: uint32(info.Mode().Perm()), UID: uid, GID: gid,
		ModifiedAt: info.ModTime().UTC().Format(time.RFC3339Nano),
	}
	switch {
	case info.Mode().IsRegular():
		entry.Type, entry.Size = "file", info.Size()
	case info.IsDir():
		entry.Type = "directory"
	case info.Mode()&os.ModeSymlink != 0:
		entry.Type = "symlink"
	default:
		return entry, fmt.Errorf("%w: devices, FIFOs and sockets cannot be transferred", ErrUnsupported)
	}
	if info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return entry, fmt.Errorf("%w: special permission bits require an administrator-managed transfer", ErrUnsupported)
	}
	return entry, nil
}
