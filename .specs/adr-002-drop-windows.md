---
title: ADR 002 Drop Windows Support Entirely
date: 2026-09-22
status: accepted
tags:
  - syncthis
  - project
  - adr
  - windows
  - packaging
---

# ADR 002: Drop Windows support entirely

- **Status:** Accepted
- **Date:** 2026-09-22
- **Decision owners:** Yoshi

## Context

Syncthis targets personal file synchronization across devices. The initial release scope included support for Linux, macOS, and other platforms where Tailscale runs. The specification included Windows support as a future goal, but operational constraints and measured priorities have shifted focus.

The build system, packaging, and testing infrastructure were designed with multi-platform support in mind, but the actual development effort and maintenance burden of Windows support extends beyond the current prototype timeline.

## Decision

**Remove Windows support entirely from all release artifacts, packaging, build documentation, and the specification.** This applies to:

- The Nix flake and module definitions
- Archive-based packaging
- Build scripts and CI/CD targets
- Specification and documentation
- Non-goals and scope statements

Syncthis will be packaged for Linux and macOS only. Both are supported through the Nix flake (the primary install path) and archive-based distributions for platforms without Nix.

## Consequences

### Positive

- Simplified packaging and build infrastructure; no multi-platform branching logic specific to Windows.
- Reduced test matrix and CI/CD complexity.
- Clearer scope and messaging for prospective users.
- Build and development time focused on the LAN-first, Linux-first target platforms.

### Negative

- Users on Windows cannot run Syncthis natively.
- Any future Windows port must start from scratch and decides Windows support again as a separate decision.
- Breaking change for any user who anticipated Windows support in a future release.

## Alternatives considered

### Windows support in v1

Rejected because it requires:

- Native Windows filesystem watching beyond what `fsnotify` provides natively.
- Windows-specific testing and packaging in the Nix flake and archive distributions.
- Windows CI/CD runners and a broader test matrix.
- Maintenance of Windows-specific code paths in the daemon and CLI.

The effort is not justified by the current target (Tailscale-connected personal devices on Linux and macOS), and users who need Windows sync have other options (Syncthing, Obsidian Sync, and cloud-backed solutions all support Windows).

### Deferred to a future release

Rejected because leaving it in the spec as "future work" signals availability and invites feature requests and contribution attempts. A clean removal is clearer and more maintainable.

## Related documents

- [[syncthis-spec]] - Updated to remove Windows from initial and future scope
- [[tasks.md]] - Phase 7 packaging no longer mentions Windows

