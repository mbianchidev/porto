# Product

<!-- impeccable:product-schema 1 -->

## Platform

desktop and web

## Users

Porto is primarily for a solo developer or home-lab owner managing runnable projects and services on one development machine, NAS, or home system.

## Product Purpose

Porto gives one place to discover, start, stop, inspect, and troubleshoot local or self-hosted projects, containers, Kubernetes workloads, and Linux virtual machines. The desktop app and web dashboard should make it immediately clear what is running, unhealthy, or needs attention, then provide fast operational controls without requiring the user to remember process IDs, ports, commands, contexts, or log locations.

## Positioning

Porto combines project discovery, process supervision, Docker-compatible access, Compose, local Kubernetes, Linux VMs, stable port assignment, logs, branch-aware instances, and friendly local HTTPS routing in one local-first control plane. It manages heterogeneous projects from their existing repository conventions and standard runtime formats instead of requiring each project to adopt a Porto-specific format.

## Operating Context

The user opens Porto while developing, maintaining a home server, or diagnosing a workload that failed to start. The primary surface is a dense desktop control board with a stable resource rail, inventory, and inspector. Common tasks include checking health, starting or stopping projects and containers, reading logs, switching branches, debugging pods, managing local networks and storage, and creating disposable Linux VMs.

## Capabilities and Constraints

- Porto consists of a Go CLI and daemon, a shared React web dashboard, and a desktop shell.
- Projects may use Make, Docker Compose, Node.js, Python, Go, or Rust run strategies.
- Optional providers expose Docker resources, Kubernetes contexts and Porto-created clusters, and Lima-backed Linux VMs.
- The dashboard must represent starting, running, stopped, and crashed states clearly.
- Project controls can change processes, branches, ports, worktrees, and persisted logs, so destructive or disruptive actions need explicit feedback.
- The interface must remain effective across small and large project collections without becoming a flat wall of controls.

## Brand Commitments

The product name is Porto. Product language should be direct, technical, calm, and useful to an operator without imitating a generic enterprise cloud console.

## Evidence on Hand

The repository contains the working dashboard, daemon API, CLI, project-state model, process logs, branch management, and integration settings. No customer claims, usage metrics, testimonials, or external brand assets should be invented.

## Product Principles

- Surface operational health before configuration.
- Keep common actions immediate and advanced detail progressive.
- Explain failures with actionable context rather than silent state changes.
- Respect existing project conventions and avoid unnecessary setup.
- Favor trustworthy, local-first behavior over ornamental complexity.
