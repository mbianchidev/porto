package deliverysource

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/localtools"
	"github.com/mbianchidev/porto/internal/networking"
	"github.com/mbianchidev/porto/internal/operations"
	"github.com/mbianchidev/porto/internal/runtimes"
)

type Git struct {
	Repository string `json:"repository"`
	Ref        string `json:"ref"`
	Path       string `json:"path"`
}

type Checkout struct {
	Directory string
	Commit    string
}

var validRef = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,255}$`)
var commitID = regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`)
var fetchSlots = make(chan struct{}, 2)

func ValidateHTTPS(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("repository must be a credential-free HTTPS URL; provide private access through the credential store")
	}
	return nil
}

func (source Git) Validate() error {
	if len(source.Repository) > 4096 || len(source.Path) > 4096 {
		return errors.New("Git repository or selected file path exceeds its limit")
	}
	if !validRef.MatchString(source.Ref) || strings.Contains(source.Ref, "..") || strings.HasSuffix(source.Ref, "/") || strings.HasSuffix(source.Ref, ".lock") {
		return errors.New("an explicit valid Git branch, tag or commit ref is required")
	}
	if filepath.IsAbs(source.Repository) {
		if strings.HasPrefix(source.Repository, `\\`) {
			return errors.New("network filesystem Git sources are not supported")
		}
	} else if err := ValidateHTTPS(source.Repository); err != nil {
		return err
	}
	if source.Path == "" || filepath.IsAbs(source.Path) || strings.Contains(source.Path, "\\") || strings.HasPrefix(filepath.Clean(source.Path), "..") {
		return errors.New("source file must be a confined repository-relative path")
	}
	return nil
}

func Fetch(ctx context.Context, root string, source Git, token string, runner localtools.Runner) (Checkout, error) {
	if err := source.Validate(); err != nil {
		return Checkout{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	select {
	case fetchSlots <- struct{}{}:
		defer func() { <-fetchSlots }()
	case <-ctx.Done():
		return Checkout{}, ctx.Err()
	}
	if runner == nil {
		runner = localtools.ExecRunner{}
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return Checkout{}, err
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return Checkout{}, err
	}
	work, err := os.MkdirTemp(root, ".git-")
	if err != nil {
		return Checkout{}, err
	}
	defer os.RemoveAll(work)
	home := filepath.Join(work, "home")
	if err := os.MkdirAll(filepath.Join(home, "hooks"), 0o700); err != nil {
		return Checkout{}, err
	}
	if err := config.WriteFileAtomic(filepath.Join(home, "gitconfig"), nil, 0o600); err != nil {
		return Checkout{}, err
	}
	environment := localtools.IsolatedEnvironment(home)
	if !filepath.IsAbs(source.Repository) {
		policy, err := networking.Default()
		if err != nil {
			return Checkout{}, err
		}
		network, err := policy.Environment()
		if err != nil {
			return Checkout{}, operations.Failure("Unable to apply Porto network/proxy settings to the Git source. Check the networking settings and their credential store.")
		}
		environment = append(environment, network...)
	}
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if strings.HasPrefix(name, "GIT_") {
			environment = append(environment, name+"=")
		}
	}
	environment = append(environment, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+filepath.Join(home, "gitconfig"), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_COUNT=0")
	if token != "" {
		if filepath.IsAbs(source.Repository) {
			return Checkout{}, errors.New("credentials cannot be attached to a local repository source")
		}
		authorization := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		environment = append(environment, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http."+source.Repository+".extraheader", "GIT_CONFIG_VALUE_0=Authorization: Basic "+authorization)
	}
	prefix := []string{"-c", "credential.helper=", "-c", "core.longpaths=true", "-c", "core.hooksPath=" + filepath.Join(home, "hooks"), "-c", "protocol.ext.allow=never", "-c", "http.followRedirects=false"}
	if filepath.IsAbs(source.Repository) {
		prefix = append(prefix, "-c", "protocol.file.allow=always")
	} else {
		prefix = append(prefix, "-c", "protocol.file.allow=never")
	}
	run := func(directory string, arguments ...string) ([]byte, error) {
		output, err := runner.Run(ctx, runtimes.Command{Name: "git", Dir: directory, Args: append(append([]string{}, prefix...), arguments...), Env: environment})
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, operations.Failure("Git source operation failed. Check the exact repository/ref, network access and its stored access token; no source or credential output was retained.")
		}
		return output.Stdout, nil
	}
	checkout := filepath.Join(work, "checkout")
	if _, err := run(work, "clone", "--depth=1", "--filter=blob:none", "--no-checkout", "--no-recurse-submodules", "--no-local", "--", source.Repository, checkout); err != nil {
		return Checkout{}, err
	}
	if _, err := run(checkout, "fetch", "--depth=1", "--no-tags", "--no-recurse-submodules", "origin", source.Ref); err != nil {
		return Checkout{}, err
	}
	if _, err := run(checkout, "checkout", "--detach", "FETCH_HEAD"); err != nil {
		return Checkout{}, err
	}
	output, err := run(checkout, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return Checkout{}, err
	}
	commit := strings.TrimSpace(string(output))
	if !commitID.MatchString(commit) {
		return Checkout{}, errors.New("Git returned an invalid immutable commit identity")
	}
	repository := sha256.Sum256([]byte(source.Repository))
	destination := filepath.Join(root, hex.EncodeToString(repository[:8])+"-"+commit)
	if _, err := os.Stat(destination); err == nil {
		output, err := run(destination, "status", "--porcelain", "--untracked-files=all")
		if err != nil {
			return Checkout{}, err
		}
		if len(strings.TrimSpace(string(output))) != 0 {
			return Checkout{}, operations.Failure("Recorded Git source has local edits. Restore or explicitly detach the source before reconciling; edits were not overwritten.")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(checkout, destination); err != nil {
			return Checkout{}, err
		}
	} else {
		return Checkout{}, err
	}
	selected := filepath.Join(destination, filepath.FromSlash(source.Path))
	resolved, err := filepath.EvalSymlinks(selected)
	if err != nil {
		return Checkout{}, fmt.Errorf("resolve selected source file: %w", err)
	}
	relative, err := filepath.Rel(destination, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return Checkout{}, errors.New("selected source file escapes its recorded Git checkout")
	}
	return Checkout{Directory: destination, Commit: commit}, nil
}
