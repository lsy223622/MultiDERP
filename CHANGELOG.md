# Changelog

## [Unreleased]

## [1.0.2] - 2026-09-04

- Automatically creates a missing `config.yaml` from the bundled example with
  default settings and an empty `tailnets` list.

## [1.0.1] - 2026-09-04

- Added repeatable OAuth advertised tags to the CLI, admin protocol, YAML
  configuration, and tsnet verifier setup.
- Added periodic read-only hardening validation with fail-closed admission and
  immediate repair on detected drift.
- Standardized the default backend listener on TCP 3377 and documented the
  separate public DERP endpoint and STUN port.
- Added build version metadata, third-party notices, security guidance, and
  release-focused CI pinning.

## [1.0.0] - 2026-09-04

- Initial release of the MultiDERP daemon and its upstream `derper`-based
  multi-Tailnet admission architecture.
- Added isolated per-Tailnet verifier lifecycle management, fail-closed
  admission control, health checks, and persistent state handling.
- Added web, OAuth, and auth-key enrollment through the admin CLI and YAML
  configuration.
- Added Docker/Compose deployment examples, digest-pinned release inputs, and
  stable release image publishing to GHCR.
