package vm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/acceleration"
	"github.com/mbianchidev/porto/internal/process"
	"github.com/mbianchidev/porto/internal/runtimes"
	"gopkg.in/yaml.v3"
)

type isolatedVMPolicyRunner struct{ runtimes.ExecRunner }

func (runner isolatedVMPolicyRunner) Run(ctx context.Context, command runtimes.Command) ([]byte, error) {
	if filepath.Base(command.Name) == "limactl" && len(command.Args) > 0 && command.Args[0] == "create" {
		command.Args = append([]string{"create", "--mount-none", "--containerd=none"}, command.Args[1:]...)
	}
	return runner.ExecRunner.Run(ctx, command)
}

func TestNativeStandaloneArchitecturePolicies(t *testing.T) {
	if os.Getenv("PORTO_TEST_NATIVE_STANDALONE_POLICIES") != "1" {
		t.Skip("set PORTO_TEST_NATIVE_STANDALONE_POLICIES=1 to authorize disposable VZ and foreign QEMU VMs")
	}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Fatal("native standalone architecture acceptance requires Apple Silicon macOS")
	}
	root, err := os.MkdirTemp("/tmp", "porto-vm-policies-")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PORTO_HOME", filepath.Join(root, "porto"))
	t.Setenv("LIMA_HOME", filepath.Join(root, "lima"))
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	runner := isolatedVMPolicyRunner{}
	architecture := acceleration.New(filepath.Join(root, "porto"), runner)
	host := architecture.ProbeHost(ctx)
	if !host.Virtualization.Supported || !host.Rosetta.Supported {
		t.Fatalf("real VZ/Rosetta capability is required, not a substitute backend: %+v", host)
	}
	if _, err := acceleration.SelectVM(host, acceleration.QEMUCompatible, "amd64", "qemu", false); err != nil {
		t.Fatal(err)
	}
	manager := NewWithStateDir(runner, filepath.Join(root, "metadata"))
	manager.SetAccelerationManager(architecture)
	created := []string{}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Minute)
		defer stop()
		for index := len(created) - 1; index >= 0; index-- {
			if err := manager.Delete(cleanup, created[index], true); err != nil {
				t.Errorf("remove only owned standalone acceptance VM %s: %v", created[index], err)
				return
			}
		}
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	source := filepath.Join(root, "synthetic.go")
	if err := os.WriteFile(source, []byte(`package main
import ("fmt"; "runtime")
func main() { fmt.Print("synthetic-original-linux-"+runtime.GOARCH) }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binaries := map[string]string{}
	for _, arch := range []string{"amd64", "arm64"} {
		path := filepath.Join(root, "probe-"+arch)
		command := process.NewCommand(ctx, repository, "go", "build", "-trimpath", "-o", path, source)
		command.Env = process.WithEnvironment(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("compile only synthetic Linux/%s probe: %v: %s", arch, err, output)
		}
		binaries[arch] = path
	}
	for _, request := range []CreateRequest{
		{Name: "synthetic-rejected-native", Image: "ubuntu-24.04", Architecture: "x86_64", VMType: "qemu", ArchitecturePolicy: acceleration.NativeOnly},
		{Name: "synthetic-rejected-rosetta", Image: "ubuntu-24.04", Architecture: "x86_64", VMType: "vz", RosettaEnabled: true},
	} {
		if _, err := manager.Create(ctx, request); err == nil {
			t.Fatalf("unsupported standalone combination was created: %+v", request)
		}
		instances, err := manager.Existing(ctx, []string{request.Name})
		if err != nil || len(instances) != 0 {
			t.Fatalf("rejected VM mutated Lima: %+v, %v", instances, err)
		}
	}
	run := func(name, arch string) {
		t.Helper()
		destination := "/tmp/porto-original-" + arch
		if err := manager.Copy(ctx, binaries[arch], name+":"+destination); err != nil {
			t.Fatal(err)
		}
		output, err := manager.Exec(ctx, name, []string{"sh", "-c", `chmod 0700 "$1"; exec "$1"`, "porto-probe", destination}, nil)
		if err != nil || strings.TrimSpace(string(output)) != "synthetic-original-linux-"+arch {
			t.Fatalf("standalone VM %s did not execute original Linux/%s ELF: %v: %s", name, arch, err, output)
		}
	}
	for _, scenario := range []struct {
		name, driver, guest, kernel, execution string
		rosetta                                bool
	}{
		{name: "synthetic-standalone-vz", driver: "vz", guest: "aarch64", kernel: "aarch64", execution: "native", rosetta: true},
		{name: "synthetic-standalone-amd64", driver: "qemu", guest: "x86_64", kernel: "x86_64", execution: "qemu"},
	} {
		started := time.Now()
		instance, err := manager.Create(ctx, CreateRequest{
			Name: scenario.name, Image: "ubuntu-24.04", Architecture: scenario.guest, VMType: scenario.driver,
			RosettaEnabled: scenario.rosetta, CPUs: 2, MemoryMiB: 2048, DiskGiB: 15, Start: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, scenario.name)
		if instance.VMType != scenario.driver || instance.Architecture != scenario.guest || instance.Execution != scenario.execution {
			t.Fatalf("standalone execution identity was misreported: %+v", instance)
		}
		output, err := manager.Exec(ctx, scenario.name, []string{"uname", "-m"}, nil)
		if err != nil || strings.TrimSpace(string(output)) != scenario.kernel {
			t.Fatalf("actual standalone guest kernel = %q, %v; want %s", output, err, scenario.kernel)
		}
		configuration, err := runner.Run(ctx, runtimes.Command{Name: "limactl", Args: []string{"list", scenario.name, "--json"}})
		if err != nil {
			t.Fatal(err)
		}
		var metadata struct {
			Directory string `json:"dir"`
		}
		if err := json.Unmarshal(configuration, &metadata); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(metadata.Directory, "lima.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		var configurationYAML struct {
			Mounts []struct{ Location string } `yaml:"mounts"`
		}
		if err := yaml.Unmarshal(data, &configurationYAML); err != nil || len(configurationYAML.Mounts) != 0 {
			t.Fatalf("standalone fixture unexpectedly shared host directories: %v: %s", err, data)
		}
		if scenario.rosetta {
			probe, err := manager.Exec(ctx, scenario.name, []string{"sh", "-c", acceleration.GuestProbeScript}, nil)
			if err != nil {
				t.Fatal(err)
			}
			guestHost := host
			guestHost.VMType = "vz"
			capabilities, err := acceleration.WithGuestProbe(guestHost, probe)
			if err != nil || capabilities.AMD64Handler != "rosetta" || !capabilities.Rosetta.Supported {
				t.Fatalf("standalone VZ VM has no verified Rosetta handler: %+v, %v", capabilities, err)
			}
			run(scenario.name, "amd64")
			run(scenario.name, "arm64")
		} else {
			run(scenario.name, "amd64")
		}
		sentinel := "synthetic-preserved-" + scenario.name
		output, err = manager.Exec(ctx, scenario.name, []string{"sh", "-c", `cat > "$HOME/.porto-policy-sentinel"; sync; cat "$HOME/.porto-policy-sentinel"`}, []byte(sentinel))
		if err != nil || string(output) != sentinel {
			t.Fatalf("persistent standalone sentinel was not written before stop: %q, %v", output, err)
		}
		t.Logf("verified %s guest kernel and original ELF execution; persistent home sentinel written before stop", scenario.name)
		if err := manager.Stop(ctx, scenario.name); err != nil {
			t.Fatal(err)
		}
		if err := manager.Start(ctx, scenario.name); err != nil {
			t.Fatal(err)
		}
		output, err = manager.Exec(ctx, scenario.name, []string{"sh", "-c", `cat "$HOME/.porto-policy-sentinel"`}, nil)
		if err != nil || string(output) != sentinel {
			t.Fatalf("standalone policy lifecycle did not preserve the guest home sentinel: %q, %v", output, err)
		}
		if err := manager.Stop(ctx, scenario.name); err != nil {
			t.Fatal(err)
		}
		t.Logf("actual standalone VM host=%s/%s Lima=%s driver=%s kernel=%s execution=%s Rosetta=%t create/boot/ELF/restart/disk preservation=%s",
			host.HostOS, host.HostArchitecture, host.LimaVersion, scenario.driver, scenario.kernel, scenario.execution, scenario.rosetta, time.Since(started))
	}
	t.Log("standalone VZ/Rosetta and full-system amd64 QEMU acceptance passed")
}
