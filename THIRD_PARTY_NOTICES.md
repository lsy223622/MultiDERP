# Third-party notices

UniDERP includes and distributes code from the upstream Tailscale project
under the BSD 3-Clause License and the accompanying patent grant.

- Dependency: `tailscale.com v1.102.3`
- Upstream source: <https://github.com/tailscale/tailscale>
- Upstream commit recorded for this release line:
  `53a0d659afa51835dd7a9283873cca44261454f8`
- License text: [licenses/tailscale/LICENSE](licenses/tailscale/LICENSE)
- Patent text: [licenses/tailscale/PATENTS](licenses/tailscale/PATENTS)

The `derper` binary in the container is built from this pinned Tailscale
module with the UniDERP patch in `patches/tailscale/uniderp.patch`. The patch
adds cached device-key authorization, control-policy application, management
routes and per-tailnet byte scheduling. Its digest is recorded in
`patches/tailscale/uniderp.sha256` and `release-manifest.yaml`.

The license and patent files above are preserved verbatim from that
upstream module and are included in the runtime image under
`/usr/share/licenses/uniderp/`.

The management UI bundles the Inter font by The Inter Project Authors under
the SIL Open Font License 1.1. Source: <https://github.com/rsms/inter>.
The license is included in [licenses/Inter-LICENSE.txt](licenses/Inter-LICENSE.txt)
and in the runtime image under `/usr/share/licenses/uniderp/licenses/`.

The rest of this repository is covered by the project license in `LICENSE`.
