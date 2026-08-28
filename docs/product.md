# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

Porto is primarily for a solo developer or home-lab owner managing runnable projects and services on one development machine, NAS, or home system.

## Product Purpose

Porto gives one place to discover, start, stop, inspect, and troubleshoot local or self-hosted projects. The dashboard should make it immediately clear what is running, unhealthy, or needs attention, then provide fast operational controls without requiring the user to remember process IDs, ports, commands, or log locations.

## Positioning

Porto combines project discovery, process supervision, stable port assignment, logs, branch-aware instances, and friendly local HTTPS routing in one lightweight self-hosted tool. It manages heterogeneous projects from their existing repository conventions instead of requiring each project to adopt a Porto-specific runtime format.

## Operating Context

The user opens Porto while developing, maintaining a home server, or diagnosing a project that failed to start. The primary surface is a dense operational overview with expandable inline project detail. Common tasks include checking health, starting or stopping projects, reading logs, switching branches, preparing dependencies, and managing optional integrations.

## Capabilities and Constraints

- Porto consists of a Go CLI and daemon with a React dashboard served by the daemon.
- Projects may use Make, Docker Compose, Node.js, Python, Go, or Rust run strategies.
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
