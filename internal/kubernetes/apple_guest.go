package kubernetes

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/yaml"
)

//go:embed apple-node-bootstrap.sh
var appleNodeBootstrap string

//go:embed apple-node-prepare.sh
var appleNodePreparation string

var appleKubeletNodeIP = regexp.MustCompile(`--node-ip=[0-9.]+`)

func (p *ClusterProvisioner) waitAppleGuest(ctx context.Context, node *AppleNode) error {
	if err := waitApple(ctx, "wait for native node services "+node.ID, func(probe context.Context) error {
		_, err := p.appleGuest(probe, node.ID, 10*time.Second, nil, "/bin/sh", "-ec",
			`test "$(systemctl show --property=SystemState --value)" != stopping; systemctl is-active --quiet containerd.service; systemctl is-active --quiet dbus.service; systemctl is-active --quiet systemd-logind.service`)
		return err
	}); err != nil {
		return err
	}
	output, err := p.appleReadFile(ctx, node.ID, "/proc/sys/kernel/random/boot_id")
	if err != nil {
		return err
	}
	node.BootID = strings.TrimSpace(string(output))
	if node.BootID == "" || strings.ContainsAny(node.BootID, " \t\r\n") {
		return fmt.Errorf("native node %s returned an invalid boot identity", node.ID)
	}
	return nil
}

func (p *ClusterProvisioner) prepareAppleNetwork(ctx context.Context, node string) error {
	const script = `set -eu
sysctl -w net.ipv4.ip_forward=1
sysctl -w net.bridge.bridge-nf-call-iptables=1
sysctl -w net.bridge.bridge-nf-call-ip6tables=1
for chain in OUTPUT FORWARD; do
  if ! iptables -t mangle -C "$chain" -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --set-mss 1220 2>/dev/null; then
    iptables -t mangle -A "$chain" -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --set-mss 1220
  fi
done`
	_, err := p.appleGuest(ctx, node, 30*time.Second, nil, "/bin/sh", "-c", script)
	return err
}

func (p *ClusterProvisioner) releaseAppleKubelet(ctx context.Context, node string, changed bool) error {
	const script = `set -eu
touch /run/porto-kubelet-ready
state="$(systemctl show kubelet.service --property=ActiveState --value)"
if [ "$state" = active ]; then
  if [ "$1" = changed ]; then systemctl restart kubelet.service; fi
elif [ "$state" != activating ]; then
  systemctl start --no-block kubelet.service
fi`
	mode := "unchanged"
	if changed {
		mode = "changed"
	}
	_, err := p.appleGuest(ctx, node, 30*time.Second, nil, "/bin/sh", "-c", script, "sh", mode)
	return err
}

func appleClientConfiguration(data []byte, server, serverName string, host bool) ([]byte, []byte, error) {
	configuration, err := clientcmd.Load(data)
	if err != nil {
		return nil, nil, fmt.Errorf("decode owned native kubeconfig: %w", err)
	}
	if len(configuration.Clusters) != 1 || len(configuration.AuthInfos) != 1 || len(configuration.Contexts) != 1 {
		return nil, nil, errors.New("owned native kubeconfig must contain exactly one cluster, user and context")
	}
	var authority []byte
	for _, cluster := range configuration.Clusters {
		if cluster.InsecureSkipTLSVerify || len(cluster.CertificateAuthorityData) == 0 || cluster.CertificateAuthority != "" {
			return nil, nil, errors.New("native kubeconfig does not provide an embedded verified TLS authority")
		}
		authority = cluster.CertificateAuthorityData
		cluster.Server, cluster.TLSServerName = server, serverName
	}
	for _, user := range configuration.AuthInfos {
		if user.Exec != nil || user.AuthProvider != nil || (host && user.TokenFile != "") ||
			user.Impersonate != "" || len(user.ImpersonateGroups) > 0 {
			return nil, nil, errors.New("native kubeconfig contains an unsupported executable or alternate identity")
		}
		if host && (len(user.ClientCertificateData) == 0 || len(user.ClientKeyData) == 0 ||
			user.ClientCertificate != "" || user.ClientKey != "" || user.Token != "") {
			return nil, nil, errors.New("native host kubeconfig must embed its exact client certificate identity")
		}
	}
	normalized, err := clientcmd.Write(*configuration)
	return normalized, authority, err
}

func (p *ClusterProvisioner) pointAppleGuestClient(ctx context.Context, node, file, server, serverName string) (bool, error) {
	original, err := p.appleReadFile(ctx, node, file)
	if err != nil {
		return false, err
	}
	normalized, _, err := appleClientConfiguration(original, server, serverName, false)
	if err != nil {
		return false, err
	}
	if bytes.Equal(original, normalized) {
		return false, nil
	}
	return true, p.appleWriteFile(ctx, node, file, normalized)
}

func (p *ClusterProvisioner) pointAppleKubelet(ctx context.Context, node AppleNode) (bool, error) {
	original, err := p.appleReadFile(ctx, node.ID, "/var/lib/kubelet/kubeadm-flags.env")
	if err != nil {
		return false, err
	}
	replacement := "--node-ip=" + node.IPv4
	if !appleKubeletNodeIP.Match(original) {
		return false, errors.New("owned kubelet flags have no explicit native node address")
	}
	normalized := appleKubeletNodeIP.ReplaceAll(original, []byte(replacement))
	if bytes.Equal(original, normalized) {
		return false, nil
	}
	return true, p.appleWriteFile(ctx, node.ID, "/var/lib/kubelet/kubeadm-flags.env", normalized)
}

func (p *ClusterProvisioner) healAppleControlPlane(ctx context.Context, request *ClusterRequest, node *AppleNode) (bool, error) {
	authority, err := p.appleReadFile(ctx, node.ID, "/etc/kubernetes/pki/ca.crt")
	if err != nil || request.Apple.CAHash == "" || appleDigest(authority) != request.Apple.CAHash {
		return false, errors.Join(errors.New("native control-plane TLS authority no longer matches recorded ownership"), err)
	}
	original, err := p.appleReadFile(ctx, node.ID, "/etc/kubernetes/manifests/kube-apiserver.yaml")
	if err != nil {
		return false, err
	}
	var manifest corev1.Pod
	if err := yaml.Unmarshal(original, &manifest); err != nil {
		return false, err
	}
	found, changed := false, false
	for index := range manifest.Spec.Containers {
		container := &manifest.Spec.Containers[index]
		if container.Name != "kube-apiserver" {
			continue
		}
		for argument := range container.Command {
			if strings.HasPrefix(container.Command[argument], "--advertise-address=") {
				found = true
				replacement := "--advertise-address=" + node.IPv4
				changed = changed || container.Command[argument] != replacement
				container.Command[argument] = replacement
			}
		}
	}
	if !found {
		return false, errors.New("owned native apiserver manifest has no advertise address")
	}
	if changed {
		normalized, err := yaml.Marshal(manifest)
		if err != nil {
			return false, err
		}
		if err := p.appleWriteFile(ctx, node.ID, "/etc/kubernetes/manifests/kube-apiserver.yaml", normalized); err != nil {
			return false, err
		}
	}
	for _, file := range []string{"admin.conf", "super-admin.conf", "controller-manager.conf", "scheduler.conf", "kubelet.conf"} {
		updated, err := p.pointAppleGuestClient(ctx, node.ID, "/etc/kubernetes/"+file, "https://127.0.0.1:6443", node.ID)
		if err != nil {
			return false, err
		}
		changed = changed || updated
	}
	updated, err := p.pointAppleKubelet(ctx, *node)
	return changed || updated, err
}

func (p *ClusterProvisioner) refreshAppleKubeconfig(ctx context.Context, request *ClusterRequest) error {
	controlPlane := appleNodeByRole(request, "control-plane")
	if controlPlane == nil || request.APIPort <= 0 {
		return errors.New("native API ownership has no control plane or stable loopback port")
	}
	data, err := p.appleReadFile(ctx, controlPlane.ID, "/etc/kubernetes/admin.conf")
	if err != nil {
		return err
	}
	server := "https://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(request.APIPort))
	normalized, authority, err := appleClientConfiguration(data, server, controlPlane.ID, true)
	if err != nil {
		return err
	}
	fingerprint := appleDigest(authority)
	if request.Apple.CAHash != "" && request.Apple.CAHash != fingerprint {
		return errors.New("native cluster TLS authority changed; refusing to trust a replacement identity")
	}
	configuration, err := clientcmd.Load(normalized)
	if err != nil {
		return err
	}
	contextName := clusterContextName(*request)
	cluster := &clientcmdapi.Cluster{}
	user := &clientcmdapi.AuthInfo{}
	for _, value := range configuration.Clusters {
		cluster = value
	}
	for _, value := range configuration.AuthInfos {
		user = value
	}
	configuration.Clusters = map[string]*clientcmdapi.Cluster{contextName: cluster}
	configuration.AuthInfos = map[string]*clientcmdapi.AuthInfo{contextName: user}
	configuration.Contexts = map[string]*clientcmdapi.Context{contextName: {Cluster: contextName, AuthInfo: contextName}}
	configuration.CurrentContext = contextName
	encoded, err := clientcmd.Write(*configuration)
	if err != nil {
		return err
	}
	encoded, err = yaml.YAMLToJSON(encoded)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(p.clusterKubeconfigPath(request.Name), encoded); err != nil {
		return err
	}
	request.Apple.CAHash = fingerprint
	return p.writeClusterMetadata(*request)
}

func appleAuthorityDiscoveryHash(data []byte) (string, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return "", errors.New("native discovery authority is not a PEM certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil || !certificate.IsCA {
		return "", errors.New("native discovery authority is not a valid CA certificate")
	}
	digest := sha256.Sum256(certificate.RawSubjectPublicKeyInfo)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func appleNodeRegistration(node AppleNode) map[string]any {
	return map[string]any{
		"name": node.ID, "criSocket": "unix:///run/containerd/containerd.sock",
		"kubeletExtraArgs": []map[string]string{
			{"name": "node-ip", "value": node.IPv4}, {"name": "fail-swap-on", "value": "false"},
		},
	}
}

func appleInitConfiguration(node AppleNode, version string) ([]byte, error) {
	address, err := netip.ParseAddr(node.IPv4)
	if err != nil || !address.Is4() {
		return nil, errors.New("native bootstrap requires a validated IPv4 address")
	}
	documents := []map[string]any{
		{
			"apiVersion": "kubeadm.k8s.io/v1beta4", "kind": "InitConfiguration",
			"localAPIEndpoint": map[string]any{"advertiseAddress": node.IPv4, "bindPort": 6443},
			"nodeRegistration": appleNodeRegistration(node),
		},
		{
			"apiVersion": "kubeadm.k8s.io/v1beta4", "kind": "ClusterConfiguration",
			"kubernetesVersion": version,
			"apiServer":         map[string]any{"certSANs": []string{node.ID, node.IPv4, "127.0.0.1"}},
			"networking":        map[string]string{"podSubnet": "10.244.0.0/16", "serviceSubnet": "10.96.0.0/12"},
			"etcd": map[string]any{"local": map[string]any{"extraArgs": []map[string]string{
				{"name": "advertise-client-urls", "value": "https://127.0.0.1:2379"},
				{"name": "listen-client-urls", "value": "https://127.0.0.1:2379"},
				{"name": "initial-advertise-peer-urls", "value": "https://127.0.0.1:2380"},
				{"name": "listen-peer-urls", "value": "https://127.0.0.1:2380"},
				{"name": "initial-cluster", "value": node.ID + "=https://127.0.0.1:2380"},
			}}},
		},
		{
			"apiVersion": "kubelet.config.k8s.io/v1beta1", "kind": "KubeletConfiguration",
			"shutdownGracePeriod": "300s", "shutdownGracePeriodCriticalPods": "60s",
		},
	}
	var result []byte
	for _, document := range documents {
		encoded, err := yaml.Marshal(document)
		if err != nil {
			return nil, err
		}
		if len(result) > 0 {
			result = append(result, []byte("---\n")...)
		}
		result = append(result, encoded...)
	}
	return result, nil
}
