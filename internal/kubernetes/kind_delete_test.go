package kubernetes

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/mbianchidev/porto/internal/vm"
)

func TestManagedKindDeletionAuthorizesOnlyItsOwnScopedDockerRequests(t *testing.T) {
	runner := newFakeRunner()
	provisioner := NewClusterProvisioner(vm.New(runner), runner, t.TempDir())
	request := ClusterRequest{Name: "renamed", RuntimeName: "original", Provider: "kind"}
	for _, metadata := range []ClusterRequest{request, {Name: "unrelated", Provider: "kind"}} {
		if err := provisioner.writeClusterMetadata(metadata); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(provisioner.clusterKubeconfigPath(request.Name), []byte(`{"current-context":"porto-renamed"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var authorized context.Context
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		if command.Name != "kind" {
			return nil, nil
		}
		var token string
		for _, entry := range command.Env {
			if value, ok := strings.CutPrefix(entry, "DOCKER_CUSTOM_HEADERS="+runtimes.ManagedContainerRemovalHeader+"="); ok {
				token = value
			}
		}
		if token == "" {
			return nil, errors.New("managed kind deletion did not transport a scoped authorization")
		}
		authorized = runtimes.WithManagedContainerRemovalToken(context.Background(), token)
		if err := provisioner.ProtectContainerRemoval(authorized, "porto-original-control-plane"); err != nil {
			t.Fatalf("owned deletion rejected: %v", err)
		}
		if err := provisioner.ProtectContainerRemoval(context.Background(), "porto-original-control-plane"); err == nil {
			t.Fatal("unscoped direct deletion became authorized")
		}
		if err := provisioner.ProtectContainerRemoval(authorized, "porto-unrelated-control-plane"); err == nil {
			t.Fatal("one cluster's deletion authorized another cluster")
		}
		return nil, nil
	}
	if err := provisioner.Delete(context.Background(), request.Name); err != nil {
		t.Fatal(err)
	}
	if authorized == nil {
		t.Fatal("kind deletion was not executed")
	}
	if _, err := os.Stat(provisioner.clusterMetadataPath("unrelated")); err != nil {
		t.Fatal("unrelated cluster ownership was removed")
	}
	if err := provisioner.writeClusterMetadata(request); err != nil {
		t.Fatal(err)
	}
	if err := provisioner.ProtectContainerRemoval(authorized, "porto-original-control-plane"); err == nil {
		t.Fatal("deletion authorization survived command completion")
	}
}

func TestFailedKindDeletionRevokesAuthorizationAndRetainsOwnership(t *testing.T) {
	runner := newFakeRunner()
	provisioner := NewClusterProvisioner(vm.New(runner), runner, t.TempDir())
	request := ClusterRequest{Name: "synthetic", Provider: "kind"}
	if err := provisioner.writeClusterMetadata(request); err != nil {
		t.Fatal(err)
	}
	var token string
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		for _, entry := range command.Env {
			if value, ok := strings.CutPrefix(entry, "DOCKER_CUSTOM_HEADERS="+runtimes.ManagedContainerRemovalHeader+"="); ok {
				token = value
			}
		}
		return nil, errors.New("synthetic kind deletion failure")
	}
	if err := provisioner.Delete(context.Background(), request.Name); err == nil {
		t.Fatal("failed deletion returned success")
	}
	if token == "" {
		t.Fatal("failed deletion did not have scoped authorization")
	}
	if err := provisioner.ProtectContainerRemoval(runtimes.WithManagedContainerRemovalToken(context.Background(), token), "porto-synthetic-control-plane"); err == nil {
		t.Fatal("failed deletion leaked active authorization")
	}
	if _, err := os.Stat(provisioner.clusterMetadataPath(request.Name)); err != nil {
		t.Fatal("failed deletion lost ownership metadata")
	}
}
