# Diagnostics and repair

Porto provides the same diagnostics through the **Diagnostics** dashboard page
and the `porto diagnose` command. The collector works inside the daemon and can
also run directly from the CLI when the daemon or dashboard cannot start.

## Run checks

```sh
porto diagnose
porto diagnose --json
```

Checks are grouped across Porto, the host, networking, container runtimes,
Kubernetes, virtual machines, and bundled tools. Results use four states:

- **healthy**: available and consistent
- **degraded**: usable, but a capability or optional integration needs attention
- **unavailable**: a required enabled component cannot be reached
- **unsafe**: ownership, disk, socket, or package state makes automatic repair unsafe

The command exits with `0` for healthy or degraded reports, `1` when required
components are unavailable, and `2` when an unsafe condition is present.

The current checks cover:

- daemon/API and dashboard availability
- Porto version, executable, bundled runtime manifest, state-directory writes,
  and free disk capacity
- daemon, HTTP router, and HTTPS router ports plus local hostname resolution
- certificate lifetime and optional portless HTTPS trust/listening state
- runtime feature gates and Lima, QEMU, kind, k9s, and k0s provider versions
- Docker socket, engine ownership marker, containerd inventory, BuildKit, and CNI
- kubeconfig parsing, Kubernetes API access, Metrics API, and Gateway API add-ons
- Lima VM provider availability

## Local diagnostic bundles

Preview a bundle before creating it:

```sh
porto diagnose bundle --preview
```

Create a private, local ZIP:

```sh
porto diagnose bundle
porto diagnose bundle --output /path/to/porto-diagnostics.zip
```

The dashboard provides the same preview and download flow. Porto never uploads a
bundle automatically. Bundles contain the structured report, settings without
stored credentials, runtime ownership metadata, the bundled tool manifest,
redacted kubeconfigs, and at most the last 256 KiB of `logs/porto.log`.

Before writing an entry, Porto redacts common password, token, authorization,
registry, proxy, Kubernetes key/certificate-data, private-key, home-directory,
username, and hostname material. Source-code files are never collected.
Unreadable optional sources become warnings in `bundle-warnings.txt`; they do
not discard the rest of the bundle. Always review the preview and ZIP before
sharing them.

## Scoped repairs

Repairs require explicit confirmation:

```sh
porto diagnose repair restart-docker-runtime --confirm
porto diagnose repair reinstall-docker-context --confirm
porto diagnose repair repair-kubernetes-addons --target CLUSTER --confirm
porto diagnose repair renew-certificates --confirm
```

The dashboard shows a repair button only on a check that advertises one and
records the result in Activity.

- **restart-docker-runtime** restarts Porto's Docker API, containerd event
  subscription, and owned tunnels. It does not delete the engine VM, images,
  containers, networks, or volumes.
- **reinstall-docker-context** updates only the named `porto` Docker context.
- **repair-kubernetes-addons** reapplies Porto-managed add-ons only after the
  cluster's ownership metadata is verified.
- **renew-certificates** regenerates Porto-managed local TLS material.

Porto refuses Docker repairs when the host engine metadata and guest ownership
marker disagree. Diagnostics never adopt, reset, or delete ambiguous external
state.
