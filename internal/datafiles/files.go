package datafiles

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"
	"unicode/utf8"
)

func List(ctx context.Context, directory, relative string, resource Resource) (Listing, error) {
	listing := Listing{
		Resource: resource, Identity: resource.Fingerprint(), Path: relative,
		ReadOnly: resource.ReadOnly, Entries: make([]Entry, 0),
	}
	if err := ValidatePath(relative); err != nil {
		return listing, err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return listing, err
	}
	defer root.Close()
	file, err := root.Open(relative)
	if err != nil {
		return listing, err
	}
	defer file.Close()
	items, err := file.ReadDir(MaxListing + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return listing, err
	}
	listing.Truncated = len(items) > MaxListing
	for _, item := range items[:min(len(items), MaxListing)] {
		if err := ctx.Err(); err != nil {
			return listing, err
		}
		name := path.Join(relative, item.Name())
		info, err := root.Lstat(name)
		if err != nil {
			return listing, fmt.Errorf("inspect directory entry: %w", err)
		}
		entry, entryErr := entryFromInfo(name, info)
		if entryErr != nil {
			entry.Type = "special"
		}
		if info.Mode()&os.ModeSymlink != 0 {
			entry.Type = "symlink"
			entry.LinkTarget, err = root.Readlink(name)
			if err != nil {
				return listing, err
			}
		}
		listing.Entries = append(listing.Entries, entry)
	}
	slices.SortFunc(listing.Entries, func(left, right Entry) int {
		if (left.Type == "directory") != (right.Type == "directory") {
			if left.Type == "directory" {
				return -1
			}
			return 1
		}
		return strings.Compare(left.Path, right.Path)
	})
	return listing, nil
}

func Read(ctx context.Context, directory, relative string, readOnly bool) (Content, error) {
	var buffer strings.Builder
	if err := Download(ctx, directory, relative, &buffer, MaxTextBytes); err != nil {
		return Content{}, err
	}
	text := buffer.String()
	if !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
		return Content{}, fmt.Errorf("%w: this is not UTF-8 text; use Download instead", ErrUnsupported)
	}
	hash := sha256.Sum256([]byte(text))
	return Content{Path: relative, Text: text, SHA256: hex.EncodeToString(hash[:]), ReadOnly: readOnly}, nil
}

func Download(ctx context.Context, directory, relative string, output io.Writer, limit int64) error {
	if err := ValidatePath(relative); err != nil {
		return err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	file, err := root.Open(relative)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: only regular files can be read", ErrUnsupported)
	}
	if info.Size() > limit {
		return fmt.Errorf("%w: maximum file size is %d bytes", ErrLimit, limit)
	}
	_, err = io.CopyN(output, contextReader{file, ctx.Err}, info.Size())
	after, statErr := file.Stat()
	if statErr == nil && (after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime())) {
		statErr = ErrConflict
	}
	return errors.Join(err, statErr)
}

func Write(ctx context.Context, directory, relative string, content []byte, expectedSHA256 string) (err error) {
	if err := ValidatePath(relative); err != nil || relative == "." {
		return errors.Join(fmt.Errorf("%w: a file path is required", ErrInvalid), err)
	}
	if len(content) > MaxUploadBytes {
		return ErrLimit
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	mode := os.FileMode(0o600)
	var original os.FileInfo
	if info, statErr := root.Lstat(relative); statErr == nil {
		if !info.Mode().IsRegular() || expectedSHA256 == "" {
			return fmt.Errorf("%w: overwrite requires a current regular-file checksum", ErrConflict)
		}
		original, mode = info, info.Mode().Perm()
		if err := checkFileDigest(ctx, root, relative, expectedSHA256); err != nil {
			return err
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	} else if expectedSHA256 != "" {
		return ErrConflict
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	temporary := path.Join(path.Dir(relative), ".porto-write-"+hex.EncodeToString(random))
	file, err := root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer func() {
		removeErr := root.Remove(temporary)
		if !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	_, writeErr := file.Write(content)
	if writeErr == nil && original != nil {
		writeErr = preserveFileOwnership(file, original)
	}
	err = errors.Join(writeErr, file.Sync(), file.Close())
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if original != nil {
		current, err := root.Lstat(relative)
		if err != nil || !os.SameFile(original, current) {
			return errors.Join(ErrConflict, err)
		}
		if err := checkFileDigest(ctx, root, relative, expectedSHA256); err != nil {
			return err
		}
		return root.Rename(temporary, relative)
	}
	// Link gives no-replace publication: a concurrently created destination is
	// never overwritten. The temporary inode is removed by the deferred cleanup.
	if err := root.Link(temporary, relative); err != nil {
		return fmt.Errorf("%w: publish new file: %w", ErrConflict, err)
	}
	return nil
}

func Delete(ctx context.Context, directory, relative, expectedSHA256 string) error {
	if err := ValidatePath(relative); err != nil || relative == "." {
		return errors.Join(fmt.Errorf("%w: resource root cannot be removed", ErrInvalid), err)
	}
	if expectedSHA256 == "" {
		return fmt.Errorf("%w: deletion requires the preview checksum", ErrInvalid)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	info, err := root.Lstat(relative)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: only previewed regular files can be deleted; use volume Empty for directory cleanup", ErrUnsupported)
	}
	if err := checkFileDigest(ctx, root, relative, expectedSHA256); err != nil {
		return err
	}
	return root.Remove(relative)
}

func checkFileDigest(ctx context.Context, root *os.Root, relative, expected string) error {
	file, err := root.Open(relative)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > MaxUploadBytes {
		return ErrLimit
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, contextReader{file, ctx.Err}); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != expected {
		return ErrConflict
	}
	return nil
}

func DiskUsage(ctx context.Context, directory string) (logical int64, allocated *int64, err error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return 0, nil, err
	}
	defer root.Close()
	seen := make(map[string]bool)
	var physical int64
	known := true
	err = walkUsage(ctx, root, ".", seen, &logical, &physical, &known)
	if known {
		allocated = &physical
	}
	return logical, allocated, err
}

func walkUsage(ctx context.Context, root *os.Root, name string, seen map[string]bool, logical, physical *int64, known *bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if key := fileIdentity(info); key != "" {
		if seen[key] {
			return nil
		}
		seen[key] = true
	}
	_, _, blocks := fileMetadata(info)
	if blocks < 0 {
		*known = false
	} else {
		*physical += blocks
	}
	if info.Mode().IsRegular() {
		*logical += info.Size()
	}
	if !info.IsDir() {
		return nil
	}
	file, err := root.Open(name)
	if err != nil {
		return err
	}
	defer file.Close()
	for {
		entries, readErr := file.ReadDir(256)
		for _, item := range entries {
			if err := walkUsage(ctx, root, path.Join(name, item.Name()), seen, logical, physical, known); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}
