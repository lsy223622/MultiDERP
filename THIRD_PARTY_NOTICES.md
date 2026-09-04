# Third-party notices

MultiDERP includes and distributes code from the upstream Tailscale project
under the BSD 3-Clause License and the accompanying patent grant.

- Dependency: `tailscale.com v1.102.3`
- Upstream source: <https://github.com/tailscale/tailscale>
- Upstream commit recorded for this release line:
  `53a0d659afa51835dd7a9283873cca44261454f8`
- License text: [licenses/tailscale/LICENSE](licenses/tailscale/LICENSE)
- Patent text: [licenses/tailscale/PATENTS](licenses/tailscale/PATENTS)

The `derper` binary in the container is built from the same pinned Tailscale
module. The license and patent files above are preserved verbatim from that
upstream module and are included in the runtime image under
`/usr/share/licenses/multiderp/`.

The rest of this repository is covered by the project license in `LICENSE`.
