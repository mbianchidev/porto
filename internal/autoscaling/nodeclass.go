package autoscaling

import (
	"encoding/json"

	"github.com/awslabs/operatorpkg/status"
	"github.com/mbianchidev/porto/internal/kubernetes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
)

var NodeClassGroupVersion = schema.GroupVersion{Group: "autoscaling.porto.dev", Version: "v1alpha1"}

func init() {
	// Karpenter's object.GVK helpers use the client-go scheme even when its
	// manager has a private scheme. Register API types once, at package init.
	addNodeClass(scheme.Scheme)
}

type PortoNodeClassSpec struct {
	Machine kubernetes.MachineSpec `json:"machine"`
}

// PortoNodeClass describes the real Lima machine offered by the local provider.
type PortoNodeClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              PortoNodeClassSpec `json:"spec"`
	Status            NodeClassStatus    `json:"status,omitempty"`
}

type NodeClassStatus struct {
	Conditions []status.Condition `json:"conditions,omitempty"`
}

type PortoNodeClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PortoNodeClass `json:"items"`
}

func (n *PortoNodeClass) GetConditions() []status.Condition  { return n.Status.Conditions }
func (n *PortoNodeClass) SetConditions(c []status.Condition) { n.Status.Conditions = c }
func (n *PortoNodeClass) StatusConditions(o ...status.ForOption) status.ConditionSet {
	return status.NewReadyConditions().For(n, o...)
}
func (n *PortoNodeClass) DeepCopyObject() runtime.Object {
	out := new(PortoNodeClass)
	data, _ := json.Marshal(n)
	_ = json.Unmarshal(data, out)
	return out
}
func (n *PortoNodeClassList) DeepCopyObject() runtime.Object {
	out := new(PortoNodeClassList)
	data, _ := json.Marshal(n)
	_ = json.Unmarshal(data, out)
	return out
}

func addNodeClass(s *runtime.Scheme) {
	s.AddKnownTypes(NodeClassGroupVersion, &PortoNodeClass{}, &PortoNodeClassList{})
	metav1.AddToGroupVersion(s, NodeClassGroupVersion)
}

const nodeClassCRD = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: portonodeclasses.autoscaling.porto.dev
spec:
  group: autoscaling.porto.dev
  scope: Cluster
  names:
    kind: PortoNodeClass
    listKind: PortoNodeClassList
    plural: portonodeclasses
    singular: portonodeclass
  versions:
    - name: v1alpha1
      served: true
      storage: true
      subresources:
        status: {}
      schema:
        openAPIV3Schema:
          type: object
          required: [spec]
          properties:
            spec:
              type: object
              required: [machine]
              properties:
                machine:
                  type: object
                  required: [cpus, memoryMiB, diskGiB]
                  properties:
                    cpus: {type: integer, minimum: 1, maximum: 32}
                    memoryMiB: {type: integer, minimum: 1024, maximum: 131072}
                    diskGiB: {type: integer, minimum: 10, maximum: 1024}
            status:
              type: object
              x-kubernetes-preserve-unknown-fields: true
`
