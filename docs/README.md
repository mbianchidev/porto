# Porto documentation

The root [README](../README.md) explains what Porto offers and provides the shortest path to a running project. Use these guides for configuration and detailed behavior.

## Get started

- [Installation](installation.md) — install a release, build from source, or configure a custom dashboard path
- [Quickstart](../README.md#quickstart) — discover projects and start the daemon
- [Daily use](daily-use.md) — keep Porto available at login or boot on a development machine or home system

## Use Porto

- [Remote environments and Edge fleets](remote-environments.md): authenticated enrollment, imported identities, scoped actions, offline behavior and revocation
- [Project management](project-management.md) — CLI commands, discovery, dependency setup, lifecycle, ports, state, and logs
- [Local runtimes](runtimes.md) — Docker endpoint, containers, Compose, Kubernetes resources, pod inspection, and Linux VMs
- [Kubernetes upgrades](kubernetes-upgrades.md): distribution-specific upgrades, persistent-volume restoration, and recovery
- [Kubernetes autoscaling](kubernetes-autoscaling.md): real local Karpenter workers, KEDA triggers, budgets, and ownership
- [Porto Docker Engine](docker-engine.md) — Docker context setup, native containerd architecture, API compatibility, persistence, and limitations
- [Runtime data management](data-management.md) — deep inspection, protected storage cleanup, local backups, migration, and native filesystem capabilities
- [Branch management](branch-management.md) — branch switching, concurrent instances, managed worktrees, and cleanup
- [Local networking](networking.md) — project hostnames, HTTP and HTTPS routing, certificates, DNS, and custom forwarding

## Integrations

- [Permission-scoped extensions](extensions.md): verified installation, grants, isolated actions/views, reference integration and signed discovery
- [Windows development integration](windows-integration.md): selected WSL tooling,
  shared paths, watch policy, native credentials, ConPTY and validation boundaries
- [Optional integrations](integrations.md) — sql-not-so-lite, KillSwitch, and Sendbox
- [KillSwitch integration](kill-switch.md) — detailed requirements, commands, port ownership, and troubleshooting

## Product and design

- [Product overview](product.md) — intended users, purpose, positioning, capabilities, and principles
- [Design system](design.md) — visual language, layout, components, and interaction rules

## Develop and release

- [Continuous integration and releases](ci-cd.md) — local validation, GitHub Actions, release steps, and artifacts
