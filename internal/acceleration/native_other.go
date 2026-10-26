//go:build !darwin

package acceleration

func nativeCapabilities() (Capability, Capability) {
	return Capability{Reason: "Apple Virtualization.framework is available only on macOS"},
		Capability{Reason: "Rosetta Linux translation requires Apple Silicon and a native arm64 VZ guest"}
}
