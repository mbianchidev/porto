package nativefiles

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/net/webdav"
)

func TestNativeWebDAVUsesConfinedDirectFilesAndPreservesExistingPermissions(t *testing.T) {
	directory := t.TempDir()
	name := filepath.Join(directory, "synthetic-\u03b4.txt")
	if err := os.WriteFile(name, []byte("original"), 0o640); err != nil {
		t.Fatal(err)
	}
	client := nativeFixtureClient(t, directory, false)
	filesystem := &sftpWebDAV{client: client, verify: func(context.Context) error { return nil }}
	handler := nativeWebDAVHandler("synthetic-attachment", "synthetic-private-password", filesystem)
	for _, request := range []struct {
		method, name, body string
		status             int
	}{
		{http.MethodGet, "/synthetic-attachment/synthetic-%CE%B4.txt", "", http.StatusOK},
		{http.MethodPut, "/synthetic-attachment/synthetic-%CE%B4.txt", "direct edit", http.StatusCreated},
		{"MKCOL", "/synthetic-attachment/nested", "", http.StatusCreated},
		{http.MethodPut, "/synthetic-attachment/nested/new.txt", "new file", http.StatusCreated},
	} {
		r := httptest.NewRequest(request.method, request.name, strings.NewReader(request.body))
		r.SetBasicAuth("porto", "synthetic-private-password")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, r)
		if response.Code != request.status {
			t.Fatalf("%s %s: %d %s", request.method, request.name, response.Code, response.Body.String())
		}
	}
	content, err := os.ReadFile(name)
	if err != nil || string(content) != "direct edit" {
		t.Fatalf("native writes used a detached copy: %q, %v", content, err)
	}
	info, err := os.Stat(name)
	if err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o640 {
		t.Fatalf("native overwrite reset existing permissions: %v, %v", info, err)
	}
}

func TestNativeWebDAVEnforcesAuthenticationReadonlyAndStaleIdentity(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "synthetic.txt"), []byte("synthetic contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := nativeFixtureClient(t, directory, true)
	current := true
	filesystem := &sftpWebDAV{client: client, readOnly: true, verify: func(context.Context) error {
		if !current {
			return errors.New("synthetic stale identity")
		}
		return nil
	}}
	handler := nativeWebDAVHandler("synthetic-attachment", "synthetic-private-password", filesystem)
	request := func(method, password string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/synthetic-attachment/synthetic.txt", bytes.NewReader([]byte("forbidden")))
		if password != "" {
			r.SetBasicAuth("porto", password)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := request(http.MethodGet, ""); w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), "synthetic contents") {
		t.Fatalf("native data was served without private credentials: %d %s", w.Code, w.Body.String())
	}
	if w := request(http.MethodPut, "synthetic-private-password"); w.Code < 400 {
		t.Fatalf("immutable native resource accepted a write: %d %s", w.Code, w.Body.String())
	}
	file, err := filesystem.OpenFile(context.Background(), "/synthetic.txt", os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	current = false
	if _, err := io.ReadAll(file); err == nil {
		t.Fatal("an already-open native handle bypassed stale resource identity")
	}
	if w := request(http.MethodGet, "synthetic-private-password"); w.Code < 400 {
		t.Fatalf("a reused identity remained accessible: %d", w.Code)
	}
}

func TestNativeWebDAVRejectsLinksAndRootMutationsWithoutRemovingSourceData(t *testing.T) {
	directory := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "synthetic.txt"), []byte("outside boundary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(directory, "escape")); err != nil {
		t.Skipf("host policy does not permit synthetic symlinks: %v", err)
	}
	client := nativeFixtureClient(t, directory, false)
	filesystem := &sftpWebDAV{client: client, verify: func(context.Context) error { return nil }}
	if _, err := filesystem.OpenFile(context.Background(), "/escape/synthetic.txt", os.O_RDONLY, 0); err == nil {
		t.Fatal("WebDAV followed an escaping link")
	}
	if err := filesystem.RemoveAll(context.Background(), "/"); err == nil {
		t.Fatal("native root deletion was accepted")
	}
	if err := filesystem.RemoveAll(context.Background(), "/escape"); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filepath.Join(outside, "synthetic.txt")); err != nil || string(content) != "outside boundary" {
		t.Fatalf("link removal followed the link into unrelated source data: %q, %v", content, err)
	}
	var _ webdav.FileSystem = filesystem
}
