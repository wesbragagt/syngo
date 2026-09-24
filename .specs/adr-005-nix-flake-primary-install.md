---
title: ADR 005 Nix Flake as Primary Supported Install Path
date: 2026-09-22
status: accepted
tags:
  - syngo
  - project
  - adr
  - packaging
  - nix
---

# ADR 005: Nix flake as primary supported install path

- **Status:** Accepted
- **Date:** 2026-09-22
- **Decision owners:** Yoshi

## Context

Syngo is designed to run as a long-lived daemon on developer workstations, NAS devices, containers, and headless servers. Installation and configuration are critical to the user experience. The initial approach considered multiple packaging strategies:

- Direct archive distributions (tarballs) for Linux and macOS
- Distribution-specific packages (RPM, Deb, Homebrew)
- Container images (Docker, OCI)
- System service integration (systemd, launchd)
- Declarative configuration management (Ansible, Terraform)

Early development and testing revealed that the Tailscale-first user base (particularly NixOS and home-manager users) uses Nix flakes for reproducible, declarative system configuration. A NixOS module and home-manager module provide:

- Reproducible builds with locked dependencies
- Declarative service configuration (systemd unit auto-generation)
- Easy updates and rollbacks
- Seamless integration with Tailscale configuration
- Consistent behavior across development and production

The Nix flake approach handles Linux x86_64, Linux ARM64, macOS x86_64, and macOS ARM64 in a single definition.

## Decision

**Adopt the Nix flake (in `flake.nix`) as the primary supported install and configuration path for all platforms.**

Syngo will provide:

1. **A Nix flake** with:
   - A derivation building Syngo from source
   - Pinned dependencies (Go 1.26, standard library dependencies)
   - Output attributes for `defaultPackage`, `devShell`, and `packages`
   
2. **NixOS and home-manager modules** (in `nix/`):
   - `nixos-module`: systemd service, socket, and configuration
   - `home-manager-module`: user daemon configuration and shell integration (untested)

3. **Archive-based fallback for non-Nix systems**:
   - Pre-built static binaries for Linux and macOS (x86_64 and ARM64)
   - Published on the release page
   - No system-level integration; users manage the daemon themselves

4. **No distribution-specific packaging** in Phase 7:
   - No RPM, Deb, or Homebrew formulas at launch
   - Those can be added as separate decisions if demand is demonstrated
   - Nix provides the declarative configuration NixOS users expect; archive distributions serve others

## Consequences

### Positive

- **Reproducible for Nix users:** Every user with the same `flake.lock` gets the exact same build, and updates are explicit and reversible.
- **Integrated configuration:** The NixOS module generates systemd units from configuration, keeping install and daemon setup together.
- **Cross-platform in one definition:** Linux, macOS, and all architectures build from one flake without branching logic.
- **Tailscale integration:** The module can reference Tailscale configuration in `system.tailscale` or `services.tailscale` without custom plumbing.
- **Natural for the early user base:** Tailscale users on NixOS (a significant overlap) already use flakes for their entire system configuration.

### Negative

- **Users on non-NixOS systems must use archives or build from source.** This is acceptable for v1; distribution packages (Homebrew, Deb, RPM) are a future decision if demand is demonstrated.
- **Home-manager module is untested at launch.** It is published as a convenience but comes with no SLA; NixOS module is the supported path.
- **Maintenance burden of the Nix module.** Changes to systemd integration, configuration schema, or database paths require keeping the module in sync. This is mitigated by keeping the daemon's configuration format stable.
- **Lock-in risk for Nix-heavy workflows.** If Nix becomes a liability, uninstalling Syngo means removing the flake from the user's `flake.nix`. This is no worse than any other flake-based tool.

## Alternatives considered

### Homebrew, Deb, and RPM at launch

Rejected: it duplicates packaging work (one per distribution family) with no measured user demand. The archive distribution covers the immediate need; a single Nix flake serves the declared early user base (NixOS + Tailscale). Distribution packages can be added in a later release based on real user requests.

### No Nix, only archives

Rejected: the Nix user base (particularly NixOS + Tailscale users) is the declared target for v1, and they will write their own Nix packaging if the upstream does not provide it. Providing the module upstream is simpler maintenance than supporting three ad-hoc user modules.

### Docker/OCI container as primary

Rejected: Docker adds complexity for the primary use case (a daemon on a workstation or NAS), and most NixOS deployments use `pkgs.syngo` in a configuration module, not containers. Containers are useful for specific deployments (Kubernetes, compose stacks) but not the common path.

### Manual service integration (no Nix module)

Rejected: asking users to write systemd units by hand is error-prone and defeats the purpose of declarative configuration. The module provides clear, versioned examples of correct configuration.

## Phase 7 scope

Archive-based packaging for Linux x86_64, Linux ARM64, macOS x86_64, and macOS ARM64:

- [x] Build static binaries (`CGO_ENABLED=0`)
- [x] Create tarballs with the binary and a README
- [x] Publish tarballs alongside the release
- [x] Document extraction and manual daemon setup
- [x] Provide example systemd units for users who extract the archive

NixOS and home-manager integration:

- [x] Write a complete NixOS module with options for all configuration
- [x] Publish the module in the flake
- [x] Write a home-manager module (untested at launch)
- [x] Document the NixOS setup path as the primary recommendation
- [ ] Test the home-manager module end-to-end (deferred)

## Related documents

- [[syngo-spec]] - Phase 7 packaging section
- [[tasks.md]] - Phase 7 multi-device hardening scope

