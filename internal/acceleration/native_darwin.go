package acceleration

import (
	"runtime"
	"sync"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

var virtualizationFramework struct {
	sync.Once
	err error
}

func nativeCapabilities() (Capability, Capability) {
	virtualizationFramework.Do(func() {
		// Objective-C class registrations need the framework for the process lifetime.
		_, virtualizationFramework.err = purego.Dlopen(
			"/System/Library/Frameworks/Virtualization.framework/Virtualization", purego.RTLD_LAZY|purego.RTLD_GLOBAL)
	})
	if virtualizationFramework.err != nil {
		failure := Capability{Reason: "load Apple Virtualization.framework: " + virtualizationFramework.err.Error()}
		return failure, failure
	}
	virtualization := Capability{Reason: "Apple Virtualization.framework reports that this host cannot run VMs"}
	if class := objc.GetClass("VZVirtualMachine"); class != 0 {
		virtualization.Supported = objc.Send[bool](objc.ID(class), objc.RegisterName("isSupported"))
		if virtualization.Supported {
			virtualization.Reason = ""
		}
	}
	rosetta := Capability{Reason: "Rosetta Linux translation requires Apple Silicon and macOS 13 or newer"}
	if runtime.GOARCH != "arm64" || !virtualization.Supported {
		return virtualization, rosetta
	}
	if class := objc.GetClass("VZLinuxRosettaDirectoryShare"); class != 0 {
		switch objc.Send[int64](objc.ID(class), objc.RegisterName("availability")) {
		case 1:
			rosetta.Reason = "Rosetta is not installed; explicitly run 'softwareupdate --install-rosetta', accept Apple's license, then refresh"
		case 2:
			rosetta = Capability{Supported: true}
		}
	}
	return virtualization, rosetta
}
