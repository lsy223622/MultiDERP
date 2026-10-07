# Changelog

## [2.0.0-rc.1] - 2026-10-08

- Introduces the v2 single-controller platform with accounts, encrypted
  read-only OAuth credentials, device identity synchronization and member relay
  enrollment using private-key and HTTPS domain proof.
- Adds provider approval and applicant confirmation for shared tailnets,
  manual DERP map export, scoped management pages, events and audit.
- Adds first-run web administrator setup with automatic sign-in.
- Records public DERP TCP and STUN UDP ports per relay for map export and
  independent endpoint probes, with owner/administrator configuration.
- Enforces absolute identity/control/key deadlines and online connection
  revocation through a patched derper policy interface and persistent cache.
- Schedules RX/TX payload bytes by owner/shared groups and tailnet weights,
  with idle borrowing, optional ceilings and bounded queues.
- Requires a separate version 2 configuration/data migration; preserve old
  data for rollback and re-enter OAuth credentials and resource grants.
- Adds Linux/amd64 controller/member deployment examples using the UniDERP
  GHCR package. Candidate tags leave stable `latest` and legacy MultiDERP images
  unchanged. Windows is build-tested, without a Windows download package.
- Completes isolated acceptance on two independent real tailnets with dedicated
  read-only OAuth, stock clients and normally distributed standard DERP maps:
  public external/manual-TLS relay transfers and online revocation/recovery on
  controller and member relays. Adds grouped, scoped management, independent
  node settings and local bandwidth ceilings, System/Light/Dark appearance,
  inline grant status and actual DERP map clipboard/download validation.
- Keeps automatic certificate issuance, competing WAN traffic and complete
  backup restoration as separate candidate acceptance gates.
- Allows first automatic certificate issuance to finish during DERP startup
  instead of stopping the child at the short manual-certificate deadline.

## [1.0.3] - 2026-09-04

- Embeds the pinned Tailscale upstream version in the main and bundled `derper`
  binaries so version reporting does not fall back to `-ERR-BuildInfo`.

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
