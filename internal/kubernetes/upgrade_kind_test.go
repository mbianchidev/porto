package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/runtimes"
)

type kindUpgradeRunner struct {
	*fakeRunner
	archive []byte
}

func (r *kindUpgradeRunner) RunStreaming(ctx context.Context, command runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
	if _, err := r.Run(ctx, command); err != nil {
		return nil, err
	}
	return nil, emit(runtimes.OutputChunk{Stream: "stdout", Data: r.archive})
}

func kindUpgradeFixture(t *testing.T, failRestore bool) (*ClusterProvisioner, *kindUpgradeRunner, *[][]byte) {
	t.Helper()
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	root := t.TempDir()
	runner := &kindUpgradeRunner{fakeRunner: newFakeRunner(), archive: []byte("synthetic volume and image archive")}
	p := NewClusterProvisioner(nil, runner, root)
	spec := ClusterRequest{Name: "dev", Provider: "kind", Version: "v1.35.2", ControlPlane: MachineSpec{CPUs: 2, MemoryMiB: 2048}, NodeGroups: []NodeGroupSpec{{Name: "workers", Count: 1}}}
	if err := p.writeClusterMetadata(spec); err != nil {
		t.Fatal(err)
	}
	kubeconfig := []byte(`{"apiVersion":"v1","kind":"Config","current-context":"porto-dev","clusters":[{"name":"porto-dev","cluster":{"server":"https://127.0.0.1:54321"}}],"contexts":[{"name":"porto-dev","context":{"cluster":"porto-dev","user":"porto-dev"}}],"users":[{"name":"porto-dev","user":{"token":"synthetic"}}]}`)
	if err := os.WriteFile(p.clusterKubeconfigPath("dev"), kubeconfig, 0o600); err != nil {
		t.Fatal(err)
	}
	var restored [][]byte
	var replacement string
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		args := strings.Join(command.Args, " ")
		switch {
		case command.Name == "kind" && strings.HasPrefix(args, "create cluster"):
			for index, arg := range command.Args {
				if arg == "--name" {
					replacement = command.Args[index+1]
				}
			}
		case command.Name == "kind" && strings.HasPrefix(args, "get nodes"):
			if replacement != "" && strings.Contains(args, replacement) {
				return []byte(replacement + "-control-plane\n" + replacement + "-worker\n"), nil
			}
			return []byte("No kind nodes found"), nil
		case strings.Contains(args, "config view --raw"):
			return kubeconfig, nil
		case strings.Contains(args, "get nodes -o json"):
			runtimeName, version := "porto-dev", "v1.35.2"
			if replacement != "" {
				runtimeName, version = replacement, "v1.36.1"
			}
			return []byte(fmt.Sprintf(`{"items":[{"metadata":{"name":%q},"status":{"nodeInfo":{"kubeletVersion":%q},"conditions":[{"type":"Ready","status":"True"}]}},{"metadata":{"name":%q},"status":{"nodeInfo":{"kubeletVersion":%q},"conditions":[{"type":"Ready","status":"True"}]}}]}`, runtimeName+"-control-plane", version, runtimeName+"-worker", version)), nil
		case strings.Contains(args, "api-resources"):
			return []byte("persistentvolumes\npersistentvolumeclaims\ndeployments.apps\n"), nil
		case strings.Contains(args, "get persistentvolumes "):
			return []byte(`{"items":[{"apiVersion":"v1","kind":"PersistentVolume","metadata":{"name":"fixture-pv","uid":"old-pv"},"spec":{"capacity":{"storage":"1Gi"},"accessModes":["ReadWriteOnce"],"storageClassName":"standard","persistentVolumeReclaimPolicy":"Delete","hostPath":{"path":"/var/local-path-provisioner/fixture-pvc"},"claimRef":{"namespace":"fixture","name":"data","uid":"old-pvc"},"nodeAffinity":{"required":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"kubernetes.io/hostname","operator":"In","values":["porto-dev-worker"]}]}]}}}}]}`), nil
		case strings.Contains(args, "get persistentvolumeclaims "):
			return []byte(`{"items":[{"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"data","namespace":"fixture","uid":"old-pvc"},"spec":{"accessModes":["ReadWriteOnce"],"storageClassName":"standard","volumeName":"fixture-pv","resources":{"requests":{"storage":"1Gi"}}}}]}`), nil
		case strings.Contains(args, "get deployments.apps "):
			return []byte(`{"items":[{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"app","namespace":"fixture","uid":"old-app","resourceVersion":"42"},"spec":{"replicas":1,"selector":{"matchLabels":{"app":"fixture"}},"template":{"metadata":{"labels":{"app":"fixture"}},"spec":{"containers":[{"name":"app","image":"fixture.invalid/app:v1","volumeMounts":[{"name":"data","mountPath":"/data"}]}],"volumes":[{"name":"data","persistentVolumeClaim":{"claimName":"data"}}]}}}}]}`), nil
		case strings.Contains(args, "images ls -q"):
			return []byte("fixture.invalid/app:v1\n"), nil
		case strings.Contains(args, "uname -m"):
			return []byte("aarch64\n"), nil
		case strings.Contains(args, "apply --server-side --field-manager=porto-upgrade"):
			restored = append(restored, append([]byte(nil), command.Stdin...))
		case strings.Contains(args, "rollout status deployment/app") && failRestore:
			return nil, errors.New("synthetic application restore failure")
		case command.StdinReader != nil:
			data, err := io.ReadAll(command.StdinReader)
			if err != nil || string(data) != string(runner.archive) {
				t.Errorf("restored data = %q, error %v", data, err)
			}
		}
		return nil, nil
	}
	return p, runner, &restored
}

func TestKindUpgradePreservesVolumesImagesAndApplications(t *testing.T) {
	p, runner, restored := kindUpgradeFixture(t, false)
	result, err := p.UpgradeCluster(context.Background(), "dev", UpgradeRequest{Version: "v1.36.1", Confirm: true})
	if err != nil {
		t.Fatal(err)
	}
	current, err := p.readClusterMetadata("dev")
	if err != nil || current.Version != "v1.36.1" || current.RuntimeName == "" || current.Phase != "" {
		t.Fatalf("upgraded metadata = %+v, %v", current, err)
	}
	if len(*restored) != 3 {
		t.Fatalf("restored %d objects, want volume, claim, application", len(*restored))
	}
	manifest := strings.Join([]string{string((*restored)[0]), string((*restored)[1]), string((*restored)[2])}, "\n")
	if strings.Contains(manifest, "old-pvc") || strings.Contains(manifest, "old-app") || !strings.Contains(manifest, "porto-"+current.RuntimeName+"-worker") {
		t.Fatalf("old identities were not rewritten: %s", manifest)
	}
	commands := ""
	for _, command := range runner.commands {
		commands += command.Name + " " + strings.Join(command.Args, " ") + "\n"
	}
	for _, expected := range []string{"stop porto-dev-worker", "images export --platform linux/arm64", "load image-archive", "cp -a - porto-" + current.RuntimeName + "-worker:/var/local-path-provisioner", "rollout status deployment/app"} {
		if !strings.Contains(commands, expected) {
			t.Fatalf("missing %q in commands:\n%s", expected, commands)
		}
	}
	if strings.Contains(commands, "delete cluster --name porto-dev\n") {
		t.Fatal("original kind data was deleted")
	}
	info, err := os.Stat(filepath.Join(result.BackupPath, "recovery.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("private backup permissions: %v, %v", info, err)
	}
}

func TestKindUpgradeFailureCanRecoverOriginalCluster(t *testing.T) {
	p, runner, _ := kindUpgradeFixture(t, true)
	_, err := p.UpgradeCluster(context.Background(), "dev", UpgradeRequest{Version: "v1.36.1", Confirm: true})
	if err == nil {
		t.Fatal("application failure must fail the upgrade")
	}
	current, err := p.readClusterMetadata("dev")
	if err != nil || current.Phase != "upgrade-failed" || current.Version != "v1.35.2" {
		t.Fatalf("failed metadata = %+v, %v", current, err)
	}
	if err := p.RecoverUpgrade(context.Background(), "dev", true); err != nil {
		t.Fatal(err)
	}
	current, err = p.readClusterMetadata("dev")
	if err != nil || current.Version != "v1.35.2" || current.Phase != "" || current.RuntimeName != "" {
		t.Fatalf("recovered metadata = %+v, %v", current, err)
	}
	recovered := false
	for _, command := range runner.commands {
		if strings.Join(command.Args, " ") == "start porto-dev-control-plane" {
			recovered = true
		}
	}
	if !recovered {
		t.Fatal("original cluster was not restarted")
	}
}

func TestKindUpgradeRejectsUnsupportedVolumeAndCleansIdentities(t *testing.T) {
	var volume map[string]any
	if err := json.Unmarshal([]byte(`{"kind":"PersistentVolume","metadata":{"name":"fixture"},"spec":{"csi":{"driver":"fixture.invalid"}}}`), &volume); err != nil {
		t.Fatal(err)
	}
	if _, err := kindVolumeForUpgrade(volume, []string{"porto-dev-worker"}); err == nil {
		t.Fatal("unsupported volume accepted")
	}
	var app map[string]any
	if err := json.Unmarshal([]byte(`{"kind":"Deployment","metadata":{"name":"kube-user-app","namespace":"fixture","uid":"old","ownerReferences":[{"name":"fixture-owner","uid":"old-owner","apiVersion":"fixture.invalid/v1","kind":"Fixture"}]},"spec":{"template":{"spec":{"volumes":[{"name":"host","hostPath":{"path":"/outside"}}]}}},"status":{"readyReplicas":1}}`), &app); err != nil {
		t.Fatal(err)
	}
	if !keepUpgradeObject(app) {
		t.Fatal("owned application or user name was incorrectly excluded")
	}
	if err := validateUpgradePodVolumes(app); err == nil {
		t.Fatal("direct hostPath volume must block replacement")
	}
	cleanUpgradeObject(app)
	if app["status"] != nil || app["portoUpgradeOwners"] == nil {
		t.Fatalf("cleaned object = %+v", app)
	}
}

func TestUpgradeArchiveRejectsCorruption(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "volume.tar")
	if err := os.WriteFile(filename, []byte("synthetic archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyUpgradeArchive(filename, "invalid-checksum"); err == nil {
		t.Fatal("corrupted backup was accepted")
	}
}
