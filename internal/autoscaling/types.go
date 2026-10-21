// Package autoscaling runs host-resident upstream Karpenter controllers for
// Porto's real Lima workers and manages KEDA's in-cluster workload controllers.
package autoscaling

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/mbianchidev/porto/internal/kubernetes"
)

const (
	KarpenterVersion = "1.14.2"
	KEDAVersion      = "2.21.0"
	ownerLabel       = "autoscaling.porto.dev/owner"
	claimLabel       = "autoscaling.porto.dev/claim"
	runtimeLabel     = "autoscaling.porto.dev/runtime"
	managedName      = "porto-local"
)

// Operation must use the same exclusion gate as upgrade, stop, rename and delete.
// Pause must be called BEFORE acquiring that gate for those operations.
type Operation func(context.Context, string, string) (release func(), err error)

type Provisioner interface {
	Configuration(string) (kubernetes.ClusterRequest, error)
	ContextName(string) (string, error)
	ScaleNodeGroup(context.Context, string, kubernetes.NodeGroupSpec, string) error
	AutoscalingNodeName(context.Context, string, string) (string, error)
	AutoscalingNodeExists(context.Context, string, string) (bool, error)
	EnsureAutoscalingNode(context.Context, string, kubernetes.NodeGroupSpec, string) error
	DeleteAutoscalingNode(context.Context, string, string) error
}

type NodeConfig struct {
	Enabled  bool                   `json:"enabled"`
	MaxNodes int                    `json:"maxNodes"`
	Machine  kubernetes.MachineSpec `json:"machine"`
}

type Config struct {
	Nodes NodeConfig `json:"nodes"`
	KEDA  bool       `json:"keda"`
}

type Status struct {
	Config
	Cluster          string `json:"cluster"`
	NodeSupported    bool   `json:"nodeSupported"`
	Running          bool   `json:"running"`
	Paused           bool   `json:"paused"`
	ManagedNodes     int    `json:"managedNodes"`
	KEDAReady        bool   `json:"kedaReady"`
	KarpenterVersion string `json:"karpenterVersion"`
	KEDAVersion      string `json:"kedaVersion"`
	Message          string `json:"message,omitempty"`
}

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

func validateName(name string) error {
	if !namePattern.MatchString(name) {
		return errors.New("invalid managed cluster name")
	}
	return nil
}

func validateConfig(request kubernetes.ClusterRequest, cfg Config) error {
	if !cfg.Nodes.Enabled {
		return nil
	}
	if request.Provider != "k3s" && request.Provider != "k0s" {
		return errors.New("node autoscaling requires a k3s or k0s Lima cluster; kind topology is immutable (KEDA workload scaling is supported)")
	}
	if cfg.Nodes.MaxNodes < 1 || cfg.Nodes.MaxNodes > 32 {
		return errors.New("autoscaling maxNodes must be between 1 and 32 workers")
	}
	m := cfg.Nodes.Machine
	if m.CPUs < 1 || m.CPUs > 32 || m.MemoryMiB < 1024 || m.MemoryMiB > 131072 || m.DiskGiB < 10 || m.DiskGiB > 1024 {
		return errors.New("worker machine requires 1–32 CPUs, 1024–131072 MiB RAM and 10–1024 GiB disk")
	}
	// These aggregate limits apply to autoscaled workers, independently of the
	// explicitly provisioned control-plane and fixed worker groups.
	if cfg.Nodes.MaxNodes*m.CPUs > 128 || cfg.Nodes.MaxNodes*m.MemoryMiB > 262144 || cfg.Nodes.MaxNodes*m.DiskGiB > 4096 {
		return fmt.Errorf("autoscaled worker budget exceeds 128 CPUs, 262144 MiB RAM or 4096 GiB disk; reduce maxNodes or machine size")
	}
	return nil
}
