//go:build linux

package localhttps

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
)

func nativeBrowserArguments() []string { return []string{"--no-sandbox"} }

func nativeCertificateConsent(*testing.T, context.Context, int, string) func() error {
	return func() error { return nil }
}

func assertNativeHelperLeastPrivilege() error {
	command, err := linuxNativeCommand("systemctl", "show", "-p", "MainPID", "--value", httpsService)
	if err != nil {
		return err
	}
	output, err := command.Output()
	if err != nil {
		return err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil || pid <= 1 {
		return errors.New("HTTPS forwarder does not have an owned unprivileged process")
	}
	status, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "Uid:") {
			for _, value := range strings.Fields(line)[1:] {
				if value == "0" {
					return errors.New("HTTPS forwarder unexpectedly runs as root")
				}
			}
			return nil
		}
	}
	return errors.New("HTTPS forwarder process did not report its user identity")
}

func crashNativeHelper() error {
	return linuxCommand("systemctl", "kill", "--signal=KILL", httpsService)
}
