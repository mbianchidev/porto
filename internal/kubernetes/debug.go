package kubernetes

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	DebugToolboxImage           = "docker.io/alpine/k8s:1.36.1@sha256:692239d739589247c4a791205ed9619c28ae85a21286e19a6211c04a62c56668"
	debugToolboxLifetime        = time.Hour
	debugToolboxStartupTimeout  = 2 * time.Minute
	debugToolboxPollInterval    = time.Second
	debugToolboxContainerPrefix = "porto-debug-"
	debugToolboxHome            = "/tmp"
	debugToolboxPath            = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

type DebugContainer struct {
	Name            string `json:"name"`
	Image           string `json:"image"`
	TargetContainer string `json:"targetContainer"`
	PodUID          string `json:"podUID"`
	LifetimeSeconds int64  `json:"lifetimeSeconds"`
	Ready           bool   `json:"ready"`
	State           string `json:"state"`
	Reason          string `json:"reason,omitempty"`
	Message         string `json:"message,omitempty"`
}

type debugContainerState struct {
	Running *struct{} `json:"running"`
	Waiting *struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"waiting"`
	Terminated *struct {
		ExitCode int32  `json:"exitCode"`
		Reason   string `json:"reason"`
		Message  string `json:"message"`
	} `json:"terminated"`
}

type debugContainerProfile struct {
	Env             []json.RawMessage    `json:"env"`
	EnvFrom         json.RawMessage      `json:"envFrom,omitempty"`
	SecurityContext debugSecurityContext `json:"securityContext"`
	VolumeMounts    []debugVolumeMount   `json:"volumeMounts,omitempty"`
}

type debugSecurityContext struct {
	AllowPrivilegeEscalation bool                `json:"allowPrivilegeEscalation"`
	Capabilities             debugCapabilities   `json:"capabilities"`
	RunAsNonRoot             bool                `json:"runAsNonRoot"`
	RunAsUser                int64               `json:"runAsUser"`
	SeccompProfile           debugSeccompProfile `json:"seccompProfile"`
}

type debugCapabilities struct {
	Drop []string `json:"drop"`
}

type debugSeccompProfile struct {
	Type string `json:"type"`
}

type debugVolumeMount struct {
	Name              string  `json:"name"`
	MountPath         string  `json:"mountPath"`
	ReadOnly          bool    `json:"readOnly,omitempty"`
	RecursiveReadOnly *string `json:"recursiveReadOnly,omitempty"`
}

type debugEphemeralContainer struct {
	debugContainerProfile
	Name                     string   `json:"name"`
	Image                    string   `json:"image"`
	ImagePullPolicy          string   `json:"imagePullPolicy"`
	Command                  []string `json:"command"`
	TargetContainerName      string   `json:"targetContainerName"`
	TerminationMessagePolicy string   `json:"terminationMessagePolicy"`
}

type debugTargetSnapshot struct {
	Profile                   debugContainerProfile
	PodUID                    string
	ResourceVersion           string
	EphemeralContainersExists bool
}

type debugPatchOperation struct {
	Operation string `json:"op"`
	Path      string `json:"path"`
	Value     any    `json:"value"`
}

func (m *Manager) StartDebugContainer(
	ctx context.Context,
	contextName,
	namespace,
	pod,
	expectedPodUID,
	targetContainer string,
) (DebugContainer, error) {
	if err := validateResource(namespace, pod); err != nil {
		return DebugContainer{}, err
	}
	expectedPodUID = strings.TrimSpace(expectedPodUID)
	targetContainer = strings.TrimSpace(targetContainer)
	if expectedPodUID == "" || strings.ContainsAny(expectedPodUID, "\x00\r\n") {
		return DebugContainer{}, errors.New("pod UID is required")
	}
	if targetContainer == "" || strings.ContainsAny(targetContainer, "\x00\r\n") {
		return DebugContainer{}, errors.New("target container is required")
	}

	target, err := m.debugTargetSnapshot(ctx, contextName, namespace, pod, expectedPodUID, targetContainer)
	if err != nil {
		return DebugContainer{}, err
	}
	name, err := newDebugContainerName()
	if err != nil {
		return DebugContainer{}, err
	}
	debugContainer := debugEphemeralContainer{
		debugContainerProfile: target.Profile,
		Name:                  name,
		Image:                 DebugToolboxImage,
		ImagePullPolicy:       "IfNotPresent",
		Command: []string{
			"/bin/sh", "-c", `sleep "$1"`, "porto-debug",
			strconv.FormatInt(int64(debugToolboxLifetime/time.Second), 10),
		},
		TargetContainerName:      targetContainer,
		TerminationMessagePolicy: "File",
	}
	containerPath := "/spec/ephemeralContainers/-"
	containerValue := any(debugContainer)
	if !target.EphemeralContainersExists {
		containerPath = "/spec/ephemeralContainers"
		containerValue = []debugEphemeralContainer{debugContainer}
	}
	patch, err := json.Marshal([]debugPatchOperation{
		{Operation: "test", Path: "/metadata/uid", Value: target.PodUID},
		{Operation: "test", Path: "/metadata/resourceVersion", Value: target.ResourceVersion},
		{Operation: "add", Path: containerPath, Value: containerValue},
	})
	if err != nil {
		return DebugContainer{}, fmt.Errorf("encode Kubernetes debug toolbox patch: %w", err)
	}
	if _, err := m.run(
		ctx,
		contextName,
		debugToolboxStartupTimeout,
		nil,
		"patch", "pod", pod,
		"--namespace", namespace,
		"--subresource", "ephemeralcontainers",
		"--type", "json",
		"--patch", string(patch),
	); err != nil {
		return DebugContainer{}, fmt.Errorf("create Kubernetes debug toolbox: %w", err)
	}

	pending := DebugContainer{
		Name:            name,
		Image:           DebugToolboxImage,
		TargetContainer: targetContainer,
		PodUID:          target.PodUID,
		LifetimeSeconds: int64(debugToolboxLifetime / time.Second),
		State:           "pending",
	}
	readyContainer, err := m.waitForDebugContainer(ctx, contextName, namespace, pod, target.PodUID, name)
	if err != nil {
		pending.Message = err.Error()
		return pending, nil
	}
	return readyContainer, nil
}

func (m *Manager) debugTargetSnapshot(
	ctx context.Context,
	contextName,
	namespace,
	pod,
	expectedPodUID,
	targetContainer string,
) (debugTargetSnapshot, error) {
	detail, err := m.Pod(ctx, contextName, namespace, pod)
	if err != nil {
		return debugTargetSnapshot{}, err
	}
	var view struct {
		Metadata struct {
			UID               string `json:"uid"`
			ResourceVersion   string `json:"resourceVersion"`
			DeletionTimestamp string `json:"deletionTimestamp"`
		} `json:"metadata"`
		Spec struct {
			Containers []struct {
				Name         string             `json:"name"`
				Env          []json.RawMessage  `json:"env"`
				EnvFrom      json.RawMessage    `json:"envFrom"`
				VolumeMounts []debugVolumeMount `json:"volumeMounts"`
			} `json:"containers"`
			EphemeralContainers json.RawMessage `json:"ephemeralContainers"`
		} `json:"spec"`
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
	if err := json.Unmarshal(detail.Raw, &view); err != nil {
		return debugTargetSnapshot{}, fmt.Errorf("decode Kubernetes debug target: %w", err)
	}
	if view.Metadata.UID != expectedPodUID {
		return debugTargetSnapshot{}, fmt.Errorf(
			"pod %s/%s was replaced before the debug toolbox could start",
			namespace,
			pod,
		)
	}
	if view.Metadata.ResourceVersion == "" {
		return debugTargetSnapshot{}, errors.New("Kubernetes pod resourceVersion is unavailable")
	}
	if view.Metadata.DeletionTimestamp != "" {
		return debugTargetSnapshot{}, fmt.Errorf("pod %s/%s is terminating", namespace, pod)
	}
	if !strings.EqualFold(view.Status.Phase, "Running") {
		return debugTargetSnapshot{}, fmt.Errorf(
			"pod %s/%s must be running to start a debug toolbox",
			namespace,
			pod,
		)
	}
	for _, container := range view.Spec.Containers {
		if container.Name != targetContainer {
			continue
		}
		env, err := debugContainerEnvironment(container.Env)
		if err != nil {
			return debugTargetSnapshot{}, err
		}
		return debugTargetSnapshot{
			Profile: debugContainerProfile{
				Env:          env,
				EnvFrom:      container.EnvFrom,
				VolumeMounts: container.VolumeMounts,
				SecurityContext: debugSecurityContext{
					AllowPrivilegeEscalation: false,
					Capabilities:             debugCapabilities{Drop: []string{"ALL"}},
					RunAsNonRoot:             true,
					RunAsUser:                65534,
					SeccompProfile:           debugSeccompProfile{Type: "RuntimeDefault"},
				},
			},
			PodUID:          view.Metadata.UID,
			ResourceVersion: view.Metadata.ResourceVersion,
			EphemeralContainersExists: len(view.Spec.EphemeralContainers) > 0 &&
				string(view.Spec.EphemeralContainers) != "null",
		}, nil
	}
	return debugTargetSnapshot{}, fmt.Errorf("container %q was not found in pod %s/%s", targetContainer, namespace, pod)
}

func debugContainerEnvironment(target []json.RawMessage) ([]json.RawMessage, error) {
	environment := make([]json.RawMessage, 0, len(target)+2)
	homeFound := false
	pathFound := false
	for _, variable := range target {
		var value struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(variable, &value); err != nil {
			return nil, fmt.Errorf("decode Kubernetes debug target environment: %w", err)
		}
		switch value.Name {
		case "HOME":
			environment = append(environment, json.RawMessage(`{"name":"HOME","value":"`+debugToolboxHome+`"}`))
			homeFound = true
		case "PATH":
			environment = append(environment, json.RawMessage(`{"name":"PATH","value":"`+debugToolboxPath+`"}`))
			pathFound = true
		default:
			environment = append(environment, variable)
		}
	}
	if !homeFound {
		environment = append(environment, json.RawMessage(`{"name":"HOME","value":"`+debugToolboxHome+`"}`))
	}
	if !pathFound {
		environment = append(environment, json.RawMessage(`{"name":"PATH","value":"`+debugToolboxPath+`"}`))
	}
	return environment, nil
}

func newDebugContainerName() (string, error) {
	var suffix [5]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("generate Kubernetes debug container name: %w", err)
	}
	return debugToolboxContainerPrefix + hex.EncodeToString(suffix[:]), nil
}

func (m *Manager) DebugContainer(
	ctx context.Context,
	contextName,
	namespace,
	pod,
	expectedPodUID,
	name string,
) (DebugContainer, error) {
	if err := validateResource(namespace, pod); err != nil {
		return DebugContainer{}, err
	}
	expectedPodUID = strings.TrimSpace(expectedPodUID)
	name = strings.TrimSpace(name)
	if expectedPodUID == "" || strings.ContainsAny(expectedPodUID, "\x00\r\n") {
		return DebugContainer{}, errors.New("pod UID is required")
	}
	if !strings.HasPrefix(name, debugToolboxContainerPrefix) || strings.ContainsAny(name, "\x00\r\n") {
		return DebugContainer{}, errors.New("invalid debug container name")
	}
	detail, err := m.Pod(ctx, contextName, namespace, pod)
	if err != nil {
		return DebugContainer{}, err
	}
	if detail.Pod.UID != expectedPodUID {
		return DebugContainer{}, fmt.Errorf("pod %s/%s was replaced", namespace, pod)
	}
	var view struct {
		Spec struct {
			EphemeralContainers []struct {
				Name                string `json:"name"`
				Image               string `json:"image"`
				TargetContainerName string `json:"targetContainerName"`
			} `json:"ephemeralContainers"`
		} `json:"spec"`
		Status struct {
			EphemeralContainerStatuses []struct {
				Name  string              `json:"name"`
				State debugContainerState `json:"state"`
			} `json:"ephemeralContainerStatuses"`
		} `json:"status"`
	}
	if err := json.Unmarshal(detail.Raw, &view); err != nil {
		return DebugContainer{}, fmt.Errorf("decode Kubernetes debug toolbox: %w", err)
	}
	debugContainer := DebugContainer{
		Name:            name,
		PodUID:          expectedPodUID,
		LifetimeSeconds: int64(debugToolboxLifetime / time.Second),
		State:           "pending",
	}
	found := false
	for _, container := range view.Spec.EphemeralContainers {
		if container.Name == name {
			debugContainer.Image = container.Image
			debugContainer.TargetContainer = container.TargetContainerName
			found = true
			break
		}
	}
	for _, status := range view.Status.EphemeralContainerStatuses {
		if status.Name != name {
			continue
		}
		found = true
		switch {
		case status.State.Running != nil:
			debugContainer.Ready = true
			debugContainer.State = "running"
		case status.State.Terminated != nil:
			debugContainer.State = "terminated"
			debugContainer.Reason = status.State.Terminated.Reason
			debugContainer.Message = fmt.Sprintf(
				"Exited with code %d: %s",
				status.State.Terminated.ExitCode,
				firstNonEmpty(status.State.Terminated.Message, status.State.Terminated.Reason),
			)
		case status.State.Waiting != nil:
			debugContainer.State = "waiting"
			debugContainer.Reason = status.State.Waiting.Reason
			debugContainer.Message = firstNonEmpty(status.State.Waiting.Message, status.State.Waiting.Reason)
		}
		break
	}
	if !found {
		return DebugContainer{}, fmt.Errorf("debug container %q was not found in pod %s/%s", name, namespace, pod)
	}
	return debugContainer, nil
}

func (m *Manager) waitForDebugContainer(
	ctx context.Context,
	contextName,
	namespace,
	pod,
	expectedPodUID,
	name string,
) (DebugContainer, error) {
	waitContext, cancel := context.WithTimeout(ctx, debugToolboxStartupTimeout)
	defer cancel()
	ticker := time.NewTicker(debugToolboxPollInterval)
	defer ticker.Stop()

	for {
		debugContainer, err := m.DebugContainer(
			waitContext,
			contextName,
			namespace,
			pod,
			expectedPodUID,
			name,
		)
		if err != nil {
			return DebugContainer{}, fmt.Errorf("inspect Kubernetes debug toolbox: %w", err)
		}
		if debugContainer.Ready ||
			debugContainer.State == "terminated" ||
			(debugContainer.State == "waiting" &&
				debugContainer.Reason != "" &&
				debugContainer.Reason != "ContainerCreating" &&
				debugContainer.Reason != "PodInitializing") {
			return debugContainer, nil
		}

		select {
		case <-waitContext.Done():
			debugContainer.Message = firstNonEmpty(
				debugContainer.Message,
				fmt.Sprintf("Toolbox did not start within %s", debugToolboxStartupTimeout),
			)
			return debugContainer, nil
		case <-ticker.C:
		}
	}
}
