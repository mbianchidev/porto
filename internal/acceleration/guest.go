package acceleration

import (
	"bufio"
	"errors"
	"strings"
)

const GuestProbeScript = `set -eu
printf 'architecture=%s\n' "$(uname -m)"
root=/proc/sys/fs/binfmt_misc
if [ ! -r "$root/status" ] || ! grep -qx enabled "$root/status"; then
  printf 'binfmt=disabled\n'
  exit 0
fi
printf 'binfmt=enabled\n'
for name in rosetta qemu-x86_64 qemu-aarch64 qemu-arm qemu-i386 qemu-riscv64 qemu-ppc64le qemu-s390x; do
  file="$root/$name"
  [ -r "$file" ] || continue
  grep -qx enabled "$file" || continue
  grep -q '^flags: .*F' "$file" || continue
  interpreter="$(sed -n 's/^interpreter //p' "$file")"
  [ -n "$interpreter" ] && [ -x "$interpreter" ] || continue
  printf 'handler=%s\n' "$name"
done
`

func WithGuestProbe(host Capabilities, output []byte) (Capabilities, error) {
	host.Handlers = map[string]string{}
	rosetta, qemuAMD64, enabled := false, false, false
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		key, value, _ := strings.Cut(scanner.Text(), "=")
		switch key {
		case "architecture":
			host.GuestArchitecture = Architecture(value)
		case "binfmt":
			enabled = value == "enabled"
		case "handler":
			switch value {
			case "rosetta":
				rosetta = true
			case "qemu-x86_64":
				qemuAMD64 = true
				host.Handlers["amd64"] = "qemu"
			case "qemu-aarch64":
				host.Handlers["arm64"] = "qemu"
			case "qemu-arm":
				host.Handlers["arm"] = "qemu"
			case "qemu-i386":
				host.Handlers["386"] = "qemu"
			case "qemu-riscv64":
				host.Handlers["riscv64"] = "qemu"
			case "qemu-ppc64le":
				host.Handlers["ppc64le"] = "qemu"
			case "qemu-s390x":
				host.Handlers["s390x"] = "qemu"
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return host, err
	}
	if host.GuestArchitecture == "" {
		return host, errors.New("backend capability probe omitted its architecture")
	}
	host.Binfmt = Capability{Supported: enabled && len(host.Handlers) > 0,
		Reason: "no enabled, executable, fix-binary QEMU handler was found in this backend"}
	if host.Binfmt.Supported {
		host.Binfmt.Reason = ""
	}
	switch {
	case rosetta && qemuAMD64:
		host.AMD64Handler = "ambiguous"
		host.Rosetta = Capability{Reason: "both Rosetta and QEMU amd64 handlers are registered; apply one selected engine policy"}
	case rosetta:
		host.AMD64Handler = "rosetta"
		host.Rosetta.Supported = host.Rosetta.Supported && host.VMType == "vz" && host.GuestArchitecture == "arm64"
		if host.Rosetta.Supported {
			host.Rosetta.Reason = ""
		}
	case qemuAMD64:
		host.AMD64Handler = "qemu"
		host.Rosetta = Capability{Reason: "this backend currently uses its QEMU amd64 handler"}
	default:
		host.Rosetta = Capability{Reason: "no verified Rosetta handler is active in this backend"}
	}
	return host, nil
}
