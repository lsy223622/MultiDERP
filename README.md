English | [简体中文](README.zh-CN.md)

# UniDERP v2

UniDERP shares self-hosted Tailscale DERP relays across independent tailnets. One controller manages platform accounts, read-only device identities, relay resources and sharing grants. Each relay runs a patched `derper` that admits device keys from its own bounded policy cache and schedules traffic by tailnet. Clients use the standard DERP protocol and a manually configured DERP map.

The controller also runs a local relay, subject to the same registration and authorization rules as members. Members hold their node identity, policy and independent local administrator account; Tailnet OAuth credentials and shared resource accounts stay on the controller. Tailscale continues to manage peer identity, network policy and WireGuard encryption.

This release candidate targets Linux/amd64. The Compose examples use `ghcr.io/lsy223622/uniderp:2.0.0-rc.3`; prereleases do not move stable `latest`. Windows binaries are build-tested, but this release does not provide a Windows download package or an ARM64 image.

Isolated acceptance with published candidate images has exercised first-administrator Web setup, two independent real Tailnets with read-only OAuth and stock Tailscale applications, controller/member enrollment, confirmation-time admission, external/manual-certificate TLS forwarding, online revocation, scoped console workflows and actual DERP map clipboard/download contents. Complete stopped backups were restored with matching database, keys, node policy and certificates, then checked with fresh OAuth reads and stock relay connections. Real Let's Encrypt issuance and published-image cache reuse across restart were verified; renewal behavior was checked with controlled upstream tests rather than a near-expiry production renewal.

Four ordinary clients transferred actual data over a forced WAN DERP path. With an 8 Mbps payload budget and 8:2 owner/shared weights, each RX/TX direction measured about 8 Mbps for shared traffic alone, 6.4/1.6 Mbps under contention, and 8 Mbps for shared traffic after the owner endpoints stopped. Application bytes were measured separately and had lower, variable throughput. These isolated observations do not establish a long-term WAN throughput or availability guarantee.

```sh
docker pull ghcr.io/lsy223622/uniderp:2.0.0-rc.3
docker run --rm --entrypoint /usr/local/bin/uniderp \
  ghcr.io/lsy223622/uniderp:2.0.0-rc.3 version
```

Use the published image digest for immutable deployments. Source and CI are at [lsy223622/UniDERP](https://github.com/lsy223622/UniDERP); see [releases](https://github.com/lsy223622/UniDERP/releases) for version-specific results.

## Build

Use Go 1.27.1 and the pinned `tailscale.com v1.102.3`. Build both binaries; a stock `derper` does not implement the policy interface:

```sh
CGO_ENABLED=0 go build -trimpath -o uniderp ./cmd/uniderp
go run ./scripts/build-derper -out derper
UNIDERP_TEST_DERPER="$PWD/derper" go test ./...
go vet ./...
docker build --build-arg UNIDERP_VERSION=v2-local \
  --build-arg UNIDERP_COMMIT="$(git rev-parse HEAD)" -t uniderp:v2-local .
```

The builder verifies the upstream version, commit and module checksum, verifies and applies [the patch](patches/tailscale/uniderp.patch), then tests the patched packages. [release-manifest.yaml](release-manifest.yaml) records the upstream commit, patch digest and base-image digests. `uniderp version` reports the product version, commit, upstream version and patch ID. Local builds without release metadata report development values.

## Deploy the controller

The following commands target a Linux Docker host. Set a public DNS name with trusted TLS on TCP 443 and allow UDP 3478 for STUN. Outbound HTTPS must reach Tailscale's OAuth and device APIs and member domain endpoints. Public node domains are checked over HTTPS on port 443; private destinations require an explicit controller `allowed_node_cidrs` deployment decision.

```sh
mkdir -p data
sudo chown -R 10001:10001 data
sudo chmod 700 data
docker compose -f docker-compose.example.yaml pull
docker compose -f docker-compose.example.yaml up -d
```

[config.example.yaml](config.example.yaml) enables the controller, using `/data/controller.sqlite`, `/data/controller.key` and `/data/node`. The image runs as UID/GID 10001, with a read-only root filesystem and a private `/run/uniderp` tmpfs. Keep `/data` writable by that UID; mount the whole directory so keys, SQLite WAL files and node state persist together. Admin and health listeners remain local.

Open `https://YOUR-CONTROLLER-DOMAIN/manage/`. When no administrator is configured, the page asks you to choose the first administrator's username, password (12–72 bytes) and password confirmation, then signs you in. Later visits use the normal login page.

With no existing configuration, the daemon writes a bootstrap configuration and opens the management service before starting any relay or controller business. After creating the local administrator, choose **Configure as controller** or **Join an existing cluster**, save the public domain, listeners, public ports and TLS mode, then apply. A DERP node's Local node page accepts the controller HTTPS origin and one-time enrollment code. The controller administrator can register its built-in DERP node from Local node; this still proves the node key and public HTTPS domain and does not grant device access automatically. Explicit YAML configurations keep their existing role.

Local settings belong to that controller or DERP node's administrator. Saving persists them; applying restarts the local relay while keeping the management service and accounts available. A member can log in and leave while the controller is unavailable. Leaving closes existing relay connections, clears registration and cached permissions, and keeps the node key and local bandwidth limit. Online release also revokes existing grants; re-enrollment needs a new code, the original key and new sharing consent. Use the controller console for sharing grants, total policy budget, owner/shared weights and Tailnet rules. The local total limit is independent: each RX/TX scheduler uses the smaller of the controller budget and the local limit; zero adds no local limit. Higher controller budgets apply successfully and do not change the local setting or original cached policy. Status shows the received policy budget and actual scheduler budget separately.

For automated deployment, create a temporary `/data/admin-password` file containing a 12–72 byte password, readable only by UID 10001. Write it with a protected editor or secret provisioning tool; keep its contents out of command arguments and logs. Initialize through the local admin socket, then delete the temporary file:

```sh
docker exec uniderp uniderp controller init \
  --username admin --password-file /data/admin-password
```

Navigation has an independent Overview and Controller, Nodes and Tailnet groups, with pages determined by the deployment role and account permissions; empty groups are hidden. My account, Account management and System/Light/Dark appearance are in the bottom-left account menu. Controller administrators manage the cluster, accounts and built-in DERP node. Providers manage their own nodes and Tailnets; members manage their Tailnets and node-use permissions. An independent DERP node administrator only manages that node. Controller administrators create providers or members and can change their role, invalidating existing sessions. A provider who still owns nodes cannot be downgraded. Existing ordinary accounts that own nodes become providers on upgrade, preserving resources, passwords and sessions. Platform passwords are separate from Tailnet credentials; a Tailnet API failure does not prevent platform login.

### TLS and proxy

`tls_mode: external` means the reverse proxy terminates TLS and derper receives HTTP. `tls_mode: passthrough` means derper terminates TLS and loads its own certificate; clients may connect directly or through a TCP proxy that forwards TLS unchanged. The name refers to passing TLS through an upstream proxy to derper.

The external profile publishes DERP HTTP at `127.0.0.1:3377` and independent management HTTP at `127.0.0.1:3378`. Set `server.management.listen: ":3378"` in existing configurations to enable this second listener; new bootstrap configurations already enable it. A host reverse proxy terminates HTTPS, preserves HTTP/1.1 upgrades, and streams long-lived control responses. Route management paths directly to 3378 so the console works before relay setup and while the relay is stopped. In an existing Nginx TLS server:

```nginx
location ~ ^/(manage|api/v1|cluster/v1)/ {
    proxy_pass http://127.0.0.1:3378;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_buffering off;
    proxy_request_buffering off;
    proxy_read_timeout 3600s;
}
location / {
    proxy_pass http://127.0.0.1:3377;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_buffering off;
    proxy_request_buffering off;
    proxy_read_timeout 3600s;
}
```

The proxy needs a valid certificate and DNS configuration. If it runs in a different container, arrange a private backend network instead of treating that container's loopback as the host. STUN uses UDP 3478 directly and is not an HTTP proxy route.

Direct TLS or TCP passthrough deployments also need an independently reachable HTTPS management endpoint forwarding to 3378. Serving the console only through derper's TLS listener prevents recovery while that listener is stopped. Keep the public node domain's `/cluster/v1/domain-challenge/` reachable over HTTPS 443. The management listener is HTTP and should only be exposed through a trusted HTTPS proxy or private backend network. The Web console uploads and validates manual PEM certificates and keys, saves a complete pair under `/data`, then applies it on relay restart. It does not configure host DNS, port publishing or proxy certificates.

For direct Let's Encrypt TLS, use [docker-compose.letsencrypt.example.yaml](docker-compose.letsencrypt.example.yaml) and set:

```yaml
server:
  hostname: relay.example.com
  derp:
    listen: ":443"
    stun_listen: ":3478"
    tls_mode: passthrough
    cert_mode: letsencrypt
    cert_dir: /data/certs
```

This profile publishes TCP 80/443 and grants `NET_BIND_SERVICE`. Certificate issuance and your production reverse proxy must be verified in your deployment.

When an existing HTTP proxy terminates public TLS, route this domain's `/.well-known/acme-challenge/` requests to derper's internal HTTP port 80 so the [HTTP-01 challenge](https://letsencrypt.org/docs/challenge-types/#http-01-challenge) can reach its handler. A TLS-terminating proxy cannot pass the TLS-ALPN challenge to the backend. For a shared host, publish that HTTP listener only on a loopback port and change only the relay domain's challenge location. Keep the configured DERP TLS listener on container port 443 and normal upstream certificate/SNI validation. The first certificate can take longer than an ordinary readiness probe; automatic mode allows about two minutes for startup. Preserve all of `cert_dir`, including the ACME account key, when restarting or backing up.

For an existing certificate, use `cert_mode: manual`. Place the PEM certificate chain, including the leaf and required intermediates, in `relay.example.com.crt`, and its matching private key in `relay.example.com.key`, under `cert_dir`. The certificate must cover `server.hostname`. Make the directory accessible to UID 10001 and keep the private key readable only by that service identity. Manual certificates are loaded when derper starts; restart the node after replacing them.

Manual TLS can use a non-443 backend port:

```yaml
server:
  hostname: relay.example.com
  derp:
    listen: ":3377"
    stun_listen: ":3478"
    tls_mode: passthrough
    cert_mode: manual
    cert_dir: /data/certs
```

For example, publishing host TCP 3489 to container TCP 3377 exposes direct TLS on 3489. Set the node resource’s public DERP TCP port to 3489 in the management page. Its public STUN UDP port is configured separately. These ports default to 443/3478 and describe the host mapping or proxy entry points, not container listeners; map export and independent probes use the saved values. Owners and administrators can change them; sharers can read them. Re-export and update each tailnet’s map after a change. The controller still verifies the node's public HTTPS domain on port 443, so keep that entry point reachable too. Open the published port in both the host and cloud firewall, and serve every advertised DNS address family. Let's Encrypt mode requires the configured DERP listener on 443 and its ACME entry points; changing a port alone does not adapt that mode.

Manual TLS requires SNI matching `server.hostname`. To check a loopback backend while preserving the hostname and normal certificate validation, use:

```sh
curl --resolve relay.example.com:3489:127.0.0.1 \
  https://relay.example.com:3489/derp/probe
```

An HTTP reverse proxy can also terminate public TLS and establish a separate TLS connection to a `passthrough` backend. This differs from forwarding TLS unchanged through a TCP proxy. In the earlier Nginx location, replace the HTTP `proxy_pass` and add:

```nginx
proxy_pass https://127.0.0.1:3489;
proxy_ssl_server_name on;
proxy_ssl_name relay.example.com;
proxy_ssl_verify on;
proxy_ssl_trusted_certificate /etc/ssl/certs/ca-certificates.crt;
proxy_ssl_verify_depth 3;
```

The CA bundle path must exist inside the proxy's container or host. Nginx defaults to no upstream SNI, disabled upstream certificate verification and a verification depth of 1. A valid longer chain can fail with `certificate chain too long`; choose a depth sufficient for the deployed chain rather than disabling verification. The example uses 3. These `proxy_ssl_*` settings apply only to an HTTPS backend; the `external` HTTP backend needs none of them. See the [Nginx upstream TLS directives](https://nginx.org/en/docs/http/ngx_http_proxy_module.html#proxy_ssl_verify_depth).

## Join a relay

On the member host, copy [config.node.example.yaml](config.node.example.yaml) to `node-data/config.yaml`; set `node.controller_url` to the controller's HTTPS origin and `server.hostname` to the member's own public domain. Prepare `node-data` with the same UID 10001 and restrictive permissions, then start [docker-compose.node.example.yaml](docker-compose.node.example.yaml). Configure member TLS/proxy and STUN as above. Each example is intended for its own host; adjust ports if colocating services.

In **My nodes**, the provider creates a relay resource for that exact domain and obtains a single-use enrollment code valid for 30 minutes. Put the code in a protected `/data/enrollment-code` file on the member host:

```sh
docker exec uniderp-node uniderp node enroll \
  --controller https://control.example.com --code-file /data/enrollment-code
```

Delete the temporary file after success. Enrollment verifies possession of the node's private key and control of its HTTPS domain. Keep `/data/node/node.key`, registration and policy state persistent. To register the controller's own relay, create its resource and run the same command in container `uniderp`, using its controller HTTPS origin. Administrator status alone does not grant relay access.

## Bind and share tailnets

1. In **My Tailnet**, enter the canonical `T...` Tailnet ID from Tailscale's General settings and an OAuth client secret configured for `devices:core:read` only. UniDERP extracts the client ID from the secret, requests that read scope and synchronizes device node keys. A successful read cannot prove the original OAuth client lacks other permissions; the owner must check its configuration. See [Tailscale OAuth clients](https://tailscale.com/docs/features/oauth-clients) and [trust credential scopes](https://tailscale.com/docs/reference/trust-credentials).
2. Select a relay in the directory and request access for your tailnet. Its provider approves the request. The applicant then confirms activation. Before confirmation, the grant cannot admit devices. A provider's own tailnet becomes active directly.
3. Export the tailnet's DERP map and merge its `Regions` into `derpMap.Regions` in that tailnet's existing Tailscale policy. Preserve existing ACLs/grants, other regions and default DERP settings. Region IDs 900–999 must not collide with existing custom regions. UniDERP does not edit that policy or distribute client configuration automatically. See [custom DERP servers](https://tailscale.com/docs/reference/derp-servers).

The map advertises effective authorized resources using their saved public DERP TCP and STUN UDP ports, which default to 443 and 3478. Discovery and authorization are separate: retaining a removed map entry does not authorize a device key. Stock-client compatibility is the intended interface; the local library-client tests are not a substitute for testing stock applications in real tailnets.

## Bandwidth and expiration

Providers set a payload budget and owner/shared group weights; the initial budget is 100 Mbps with an 8:2 split. Under sustained contention that allocates approximately 80% to the owner group and 20% to the shared group. Idle capacity can be borrowed. Shared tailnets divide their group by their own weights, regardless of device/connection count. Optional group/tailnet ceilings limit borrowing. Rule changes apply without a new sharing confirmation.

RX and TX have separate byte schedulers and budgets. Rates and counters describe relay payload, excluding transport overhead; RX+TX is not one combined budget. Packet size, burst and observation interval affect short samples. This is not a bandwidth reservation for the entire host or a WAN throughput guarantee.

A device remains effective only until the earliest of its identity-retention deadline, controller-contact deadline, explicit grant expiration and actual device-key expiration. Identity retention starts at the last complete successful API refresh; failed or partial refreshes do not extend it. Control retention starts at the last successful controller heartbeat. Both initial retention settings are 24 hours and administrators can change them. Existing cached deadlines remain absolute across outages and restarts.

Online revocation closes connections when the node applies the new policy. Controller submission, node receipt and actual derper application are shown separately; a received revision is not an applied ACK. An offline relay can use its existing cache only until its original deadline. Removing a resource from the controller cannot instantly contact an offline process.

## Operate and recover

The console separates heartbeat/application state, device/grant deadlines, interval traffic rates and independent DERP/STUN probe results. A probe records its own observation time and does not prove every client's path. Participants see their own tailnet usage; providers and platform administrators have broader resource visibility. Events and audit are scoped to relevant resources and record the actual administrator actor.

Pause a relay before deleting it or changing its domain. A connected relay must ACK an empty policy first. A domain change preserves node identity, requires new HTTPS domain proof and leaves the relay paused until explicitly enabled. Update the relay's hostname configuration, DNS and TLS/proxy for the new domain; listener/TLS/hostname changes require a daemon restart after config reload. Keep the controller origin used by enrolled nodes reachable; changing a relay domain does not migrate that origin. For a copied node identity, stop duplicate processes before recovering its instance in the console. A new node process waits a complete 90-second prior-instance lease window before renewing its session, since a committed heartbeat can have a newer deadline than the node last persisted. Its cached policy keeps its original absolute deadlines; do not delete keys to bypass conflicts.

```sh
docker exec uniderp uniderp config reload
docker exec uniderp uniderp derp restart
docker stop --time 30 uniderp
```

Back up configuration and the complete persistent directory while the service is stopped, or use a consistent SQLite backup that accounts for its WAL. Preserve the controller database together with `controller.key`, node private key/registration, policy and its `.watermark`, persistent derper identity and certificates. Restoring a database without its matching encryption key cannot recover encrypted OAuth secrets. Preserve these files before diagnosing failure; do not substitute an empty database or delete node state. Local account recovery uses `controller recover --user-id ID --password-file PATH` over the protected admin socket.

## Migrate from v1

Stop the previous MultiDERP/UniDERP process and back up its complete configuration, data directory and exact image/version. Create a separate v2 data directory and `version: 2` configuration. Initialize platform accounts, re-enter read-only OAuth credentials, register relay domains and recreate sharing approval/confirmation. Export and review the new DERP maps before updating each tailnet policy.

Version 1 configuration is rejected with a migration error. Old verifier state cannot be converted to an OAuth client secret or sharing authorization and is not automatically deleted. To roll back, stop v2 and restore the old image with its original configuration/data and reviewed policy. Do not point an older binary at the v2 SQLite database.

## Release and validation

CI builds patched derper for integration tests and runs Linux race checks on both the patched upstream data path and the controller/cluster packages. Local evidence includes scoped API/browser workflows, trusted local TLS and STUN, DERP library relay/revocation, deterministic and controlled byte-scheduling tests, and actual Linux race execution. Public DNS, real OAuth, stock applications and WAN behavior remain separate acceptance requirements.

The image workflow accepts stable `vX.Y.Z` and prerelease tags such as `v2.0.0-rc.3`. A prerelease produces its explicit version image tag and does not update `latest`; stable tags update `latest` only in the new `ghcr.io/lsy223622/uniderp` package. Historical MultiDERP tags and the `ghcr.io/lsy223622/multiderp` package remain separate for v1 deployments and rollback.

UniDERP is licensed under [GNU GPL v3](LICENSE). See [SECURITY.md](SECURITY.md) for trust and reporting, [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) for the patched upstream license, and [CHANGELOG.md](CHANGELOG.md) for release history.
