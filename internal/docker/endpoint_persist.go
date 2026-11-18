package docker

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
)

const (
	EndpointLaunchDaemonLabel = "dev.mbianchi.porto.docker-endpoint"
	EndpointLaunchDaemonPath  = "/Library/LaunchDaemons/" + EndpointLaunchDaemonLabel + ".plist"
	portoBundleIdentifier     = "dev.mbianchi.porto"
)

// endpointRestoreScript recreates the canonical link only when nothing occupies the path, so a
// socket or another runtime's link that exists at boot is never replaced.
const endpointRestoreScript = `[ -e "$1" ] || [ -L "$1" ] || exec /bin/ln -s "$2" "$1"`

// endpointRestoreArguments runs a fixed system shell rather than the user-writable Porto
// executable, so the root job cannot be hijacked by replacing the application bundle.
func endpointRestoreArguments(canonicalPath, targetPath string) []string {
	return []string{"/bin/sh", "-c", endpointRestoreScript, "porto-docker-endpoint", canonicalPath, targetPath}
}

func endpointLaunchDaemon(canonicalPath, targetPath string) ([]byte, error) {
	for _, path := range []string{canonicalPath, targetPath} {
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("Docker endpoint path must be absolute: %q", path)
		}
		if strings.ContainsFunc(path, unicode.IsControl) {
			return nil, fmt.Errorf("Docker endpoint path contains control characters: %q", path)
		}
	}
	var plist bytes.Buffer
	plist.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>AssociatedBundleIdentifiers</key>
	<array>
		<string>` + portoBundleIdentifier + `</string>
	</array>
	<key>Label</key>
	<string>` + EndpointLaunchDaemonLabel + `</string>
	<key>ProgramArguments</key>
	<array>
`)
	for _, argument := range endpointRestoreArguments(canonicalPath, targetPath) {
		plist.WriteString("\t\t<string>")
		if err := xml.EscapeText(&plist, []byte(argument)); err != nil {
			return nil, err
		}
		plist.WriteString("</string>\n")
	}
	plist.WriteString(`	</array>
	<key>RunAtLoad</key>
	<true/>
</dict>
</plist>
`)
	return plist.Bytes(), nil
}

// PersistEndpoint keeps an activated canonical endpoint across reboots, which clear /var/run.
func PersistEndpoint(statePath string) (EndpointState, error) {
	state, err := ReadEndpointState(statePath)
	if err != nil {
		return EndpointState{}, fmt.Errorf("read Docker endpoint state: %w", err)
	}
	if err := installEndpointLaunchDaemon(state.CanonicalPath, state.TargetPath); err != nil {
		return EndpointState{}, err
	}
	state.Persistent = true
	if err := writeEndpointState(statePath, state); err != nil {
		return EndpointState{}, errors.Join(err, removeEndpointLaunchDaemon())
	}
	return state, nil
}
