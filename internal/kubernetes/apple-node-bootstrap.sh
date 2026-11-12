set -eu
mkdir -p /etc/containerd/conf.d /etc/systemd/system/kubelet.service.d \
  /etc/systemd/system/systemd-logind.service.d /etc/systemd/logind.conf.d \
  /etc/systemd/system.conf.d /etc/sysctl.d /etc/porto
if [ -f /etc/containerd/cri-base.json ]; then
  jq 'del(.hooks.createContainer)' /etc/containerd/cri-base.json > /tmp/porto-cri-base.json
  mv /tmp/porto-cri-base.json /etc/containerd/cri-base.json
fi
if [ ! -f /etc/porto/node-layout-v1 ]; then
printf "[plugins.'io.containerd.cri.v1.images']\nsnapshotter = \"native\"\n" > /etc/containerd/conf.d/porto-native.toml
printf '[Unit]\nConditionVirtualization=\nConditionPathExists=/usr/bin/dbus-daemon\n' > /etc/systemd/system/systemd-logind.service.d/porto.conf
printf '[Login]\nInhibitDelayMaxSec=360\n' > /etc/systemd/logind.conf.d/porto.conf
printf '[Manager]\nDefaultTimeoutStopSec=infinity\n' > /etc/systemd/system.conf.d/porto.conf
printf 'net.ipv4.ip_forward=1\nnet.bridge.bridge-nf-call-iptables=1\nnet.bridge.bridge-nf-call-ip6tables=1\n' > /etc/sysctl.d/90-porto.conf
cat > /etc/systemd/system/kubelet.service.d/20-porto.conf <<'EOF'
[Unit]
Requires=dbus.service systemd-logind.service
After=dbus.service systemd-logind.service
[Service]
ExecStartPre=/bin/sh -euc "while [ ! -f /run/porto-kubelet-ready ]; do sleep 0.2; done"
TimeoutStartSec=infinity
EOF
touch /etc/porto/node-layout-v1
fi
mount --make-rshared /
exec /sbin/init
