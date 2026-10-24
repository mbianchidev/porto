//go:build windows

package localhttps

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func nativeBrowserArguments() []string { return nil }

func nativeCertificateConsent(t *testing.T, parent context.Context, pid int, authorityPath string) func() error {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan error, 1)
	go func() {
		// Exercise the real protected-root prompt only on the disposable runner.
		// Never approve another process or a different certificate fingerprint.
		user32 := windows.NewLazySystemDLL("user32.dll")
		enumWindows := user32.NewProc("EnumWindows")
		enumChildren := user32.NewProc("EnumChildWindows")
		windowProcess := user32.NewProc("GetWindowThreadProcessId")
		className := user32.NewProc("GetClassNameW")
		windowText := user32.NewProc("SendMessageTimeoutW")
		dialogItem := user32.NewProc("GetDlgItem")
		postMessage := user32.NewProc("PostMessageW")
		readText := func(window uintptr) (string, error) {
			var buffer [16384]uint16
			var length uintptr
			result, _, err := windowText.Call(window, 0x000D, uintptr(len(buffer)),
				uintptr(unsafe.Pointer(&buffer[0])), 0x0002, 250, uintptr(unsafe.Pointer(&length)))
			if result == 0 {
				return "", fmt.Errorf("read owned certificate dialog: %w", err)
			}
			return windows.UTF16ToString(buffer[:]), nil
		}
		var text strings.Builder
		var dialogErr error
		children := windows.NewCallback(func(window, _ uintptr) uintptr {
			value, err := readText(window)
			if err != nil {
				dialogErr = err
				return 0
			}
			text.WriteString(value)
			text.WriteByte('\n')
			return 1
		})
		approved := make(map[uintptr]bool)
		callback := windows.NewCallback(func(window, _ uintptr) uintptr {
			var windowPID uint32
			windowProcess.Call(window, uintptr(unsafe.Pointer(&windowPID)))
			if int(windowPID) != pid || approved[window] {
				return 1
			}
			var class [256]uint16
			className.Call(window, uintptr(unsafe.Pointer(&class[0])), uintptr(len(class)))
			if windows.UTF16ToString(class[:]) != "#32770" {
				return 1
			}
			text.Reset()
			dialogErr = nil
			enumChildren.Call(window, children, 0)
			if dialogErr != nil {
				return 1
			}
			data, _, err := readAuthorityIdentity(authorityPath)
			if err != nil {
				dialogErr = err
				return 1
			}
			certificate, _ := pem.Decode(data)
			if !nativeConsentFingerprintMatches(text.String(), certificate.Bytes) {
				dialogErr = fmt.Errorf("refusing owned dialog without the complete synthetic CA fingerprint: %q", text.String())
				return 1
			}
			yes, _, err := dialogItem.Call(window, 6)
			if yes == 0 {
				dialogErr = fmt.Errorf("find owned certificate consent button: %w", err)
				return 1
			}
			label, err := readText(yes)
			if err != nil {
				dialogErr = err
				return 1
			}
			if !strings.EqualFold(strings.TrimSpace(strings.ReplaceAll(label, "&", "")), "Yes") {
				dialogErr = fmt.Errorf("refusing unexpected certificate consent button %q", label)
				return 1
			}
			if result, _, err := postMessage.Call(yes, 0x00F5, 0, 0); result == 0 {
				dialogErr = fmt.Errorf("approve owned synthetic certificate prompt: %w", err)
				return 1
			}
			approved[window] = true
			t.Logf("approved protected-root consent for synthetic CA in CLI process %d", pid)
			return 1
		})
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				done <- dialogErr
				return
			case <-ticker.C:
				if result, _, err := enumWindows.Call(callback, 0); result == 0 {
					done <- fmt.Errorf("inspect owned native certificate prompts: %w", err)
					return
				}
			}
		}
	}()
	return func() error {
		cancel()
		return <-done
	}
}

func nativeConsentFingerprintMatches(text string, certificate []byte) bool {
	compact := strings.Map(func(value rune) rune {
		if value >= '0' && value <= '9' || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F' {
			return value
		}
		return -1
	}, strings.ToLower(text))
	sha1Fingerprint, sha256Fingerprint := sha1.Sum(certificate), sha256.Sum256(certificate)
	return strings.Contains(compact, hex.EncodeToString(sha1Fingerprint[:])) ||
		strings.Contains(compact, hex.EncodeToString(sha256Fingerprint[:]))
}

func TestNativeCertificateConsentRequiresCompleteFingerprint(t *testing.T) {
	certificate := []byte("synthetic certificate")
	sum := sha256.Sum256(certificate)
	sha1Sum := sha1.Sum(certificate)
	fingerprint := hex.EncodeToString(sum[:])
	for _, test := range []struct {
		name, text string
		want       bool
	}{
		{"full", "Thumbprint: " + strings.ToUpper(fingerprint), true},
		{"spaced-sha1", fmt.Sprintf("Thumbprint (sha1): % X", sha1Sum), true},
		{"partial", "Thumbprint: " + fingerprint[:32], false},
		{"other", "Thumbprint: " + strings.Repeat("0", 64), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := nativeConsentFingerprintMatches(test.text, certificate); got != test.want {
				t.Fatalf("fingerprint match = %t, want %t", got, test.want)
			}
		})
	}
}

func assertNativeHelperLeastPrivilege() error {
	service, err := openHTTPSService(true)
	if err != nil {
		return err
	}
	defer service.Close()
	return requireOwnedService(service)
}

func crashNativeHelper() error {
	service, err := openHTTPSService(true)
	if err != nil {
		return err
	}
	defer service.Close()
	if err := requireOwnedService(service); err != nil {
		return err
	}
	status, err := service.Query()
	if err != nil {
		return err
	}
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, status.ProcessId)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	return windows.TerminateProcess(handle, 37)
}
