English | [简体中文](README.zh-CN.md)

# MultiDERP

MultiDERP lets one self-hosted Tailscale DERP endpoint serve several independent tailnets. It runs a dedicated `tsnet` verifier identity for each configured tailnet and uses those verifiers to decide whether a connecting node key may use the relay.

The relay itself is the upstream Tailscale `derper`, built from the same pinned Tailscale module as the verifier code. Each tailnet keeps its own identity, policy, and control-plane membership; MultiDERP shares the relay endpoint and admission path across them.

## How it fits together

```text
                         Tailscale control plane
                         ▲         ▲         ▲
                         │         │         │
                  ┌──────┴──┐ ┌────┴────┐ ┌───┴──────┐
                  │verifier │ │verifier │ │verifier  │
                  │tailnet A│ │tailnet B│ │tailnet C │
                  └──────┬──┘ └────┬────┘ └───┬──────┘
                         │          │           │
                         └──────────┼───────────┘
                                    │ node-key membership checks
                             ┌──────▼──────┐
DERP client ────────────────►│  admission  │
                             └──────┬──────┘
                                    │ allow / deny
                             ┌──────▼──────┐
                             │   derper    │
                             │ TLS + STUN  │
                             └─────────────┘
```

A verifier enters the admission pool after its Tailscale node is connected and its hardening state has been applied and read back successfully. For each DERP admission request, MultiDERP checks the connecting node key against the current eligible verifier set. A match from any eligible verifier admits the client.

Several tailnets can therefore use one relay host while their network boundaries stay independent. Tailscale still handles peer identity, ACL/Grants, WireGuard keys, and end-to-end encryption inside each tailnet.

## What MultiDERP manages

- one isolated Tailscale verifier state directory per tailnet;
- web-login, OAuth, and auth-key enrollment for verifiers;
- admission of DERP clients by node-key membership;
- an upstream `derper` child process;
- external TLS termination or direct TLS with `derper`;
- STUN on a separate UDP listener;
- local administration over a Unix socket;
- persistent verifier and orphan state;
- liveness, readiness, and startup health endpoints.

MultiDERP V1 targets the official Tailscale control plane. The current configuration surface focuses on private multi-tailnet admission; DERP mesh and upstream experimental rate/connection-limit controls are outside that V1 surface.

## Before you deploy

A normal container deployment needs:

- Docker Engine (the supplied deployment examples use Docker Compose);
- a public DNS name for the DERP endpoint;
- persistent writable storage for `/data`;
- TCP reachability to the public DERP HTTPS endpoint;
- UDP `3478` if clients should use the bundled STUN service;
- outbound connectivity from the verifier identities to Tailscale's control plane;
- administrative access to every tailnet that will advertise the custom DERP region.

The supplied Compose examples run the container as UID/GID `10001:10001` with a read-only root filesystem. The host directory mounted at `/data` therefore needs to be writable by that identity.

For source builds, the repository currently declares Go `1.26.6`.

## Deployment model 1: external TLS

This is the default configuration. A TLS terminator accepts the public HTTPS connection and forwards the DERP backend stream to MultiDERP on a private listener.

```text
Internet
   │
   │ TCP 443
   ▼
TLS terminator / compatible reverse proxy
   │
   │ plaintext DERP backend stream
   │ 127.0.0.1:3377
   ▼
MultiDERP / derper

Internet ───────── UDP 3478 ─────────► STUN
```

The repository's example Compose file binds TCP `3377` to host loopback and publishes STUN separately on UDP `3478`.

### 1. Prepare the data directory

```bash
git clone https://github.com/lsy223622/MultiDERP.git
cd MultiDERP

mkdir -p data
cp config.example.yaml data/config.yaml
```

On a Linux host with a normal bind mount, give the container identity write access:

```bash
sudo chown -R 10001:10001 data
```

Docker Desktop and other storage implementations may handle ownership differently. The requirement is simply that UID/GID `10001:10001` can create and atomically replace files under `/data`.

### 2. Set the public hostname

Edit `data/config.yaml`:

```yaml
version: 1

server:
  hostname: derp.example.com

  derp:
    listen: ":3377"
    stun_listen: ":3478"
    tls_mode: external
    cert_mode: none

  admin:
    socket: /run/multiderp/admin.sock

  health:
    listen: "127.0.0.1:9090"

storage:
  state_dir: /data
  tailnet_state_dir: /data/tailnets
  orphan_state_dir: /data/orphans

logging:
  level: info

tailnets: []
```

### 3. Start the service

```bash
docker compose -f docker-compose.example.yaml up -d
```

The example publishes:

```text
127.0.0.1:3377 -> container :3377/tcp
0.0.0.0:3478   -> container :3478/udp
[::]:3478      -> container :3478/udp
```

Port `3377` carries the plaintext backend stream in this mode, so the example keeps it on host loopback. The public endpoint belongs on the TLS terminator.

DERP uses a long-lived upgraded connection and then switches to its own protocol. Use a reverse-proxy configuration that is known to preserve that traffic correctly; ordinary HTTP proxy defaults are not automatically equivalent to a DERP-compatible backend path.

### 4. Terminate public TLS

Configure your TLS terminator for the hostname in `server.hostname` and forward the DERP backend connection to:

```text
http://127.0.0.1:3377
```

STUN remains a direct UDP service on `3478` and follows the host/network firewall path rather than the HTTP proxy path.

## Deployment model 2: direct TLS with Let's Encrypt

`tls_mode: passthrough` lets the child `derper` own public TLS.

Use a DERP configuration like this:

```yaml
server:
  hostname: derp.example.com

  derp:
    listen: ":443"
    stun_listen: ":3478"
    tls_mode: passthrough
    cert_mode: letsencrypt
    cert_dir: /data/certs
```

Then start the direct-TLS example:

```bash
docker compose -f docker-compose.letsencrypt.example.yaml up -d
```

That example publishes TCP `80` and `443`, plus UDP `3478`. The container remains non-root, and the Compose file adds only `NET_BIND_SERVICE`; it does not require `NET_ADMIN` or privileged mode to bind the privileged TCP ports.

The configuration validator enforces the TLS combinations:

| TLS mode | Certificate mode | Listener rules |
| --- | --- | --- |
| `external` | `none` | DERP backend uses a non-443 internal port; `cert_dir` is empty. |
| `passthrough` | `manual` | `cert_dir` is required; the TLS listener may use a custom port. |
| `passthrough` | `letsencrypt` | `cert_dir` is required and DERP listens on TCP `443`. |

## Add tailnets

The daemon starts cleanly with an empty `tailnets` list. Add verifier identities through the admin CLI after startup.

With the supplied Compose files, run the CLI inside the container:

```bash
docker exec multiderp multiderp tailnet list
```

### Web login

```bash
docker exec multiderp multiderp tailnet add personal
```

Web login is the default enrollment mode. The command returns a Tailscale authentication URL. Complete that login with an account that can add the verifier to the intended tailnet, then inspect its state:

```bash
docker exec multiderp multiderp tailnet status personal --verbose
```

### OAuth

Store the OAuth client secret in a file under protected persistent storage, then pass its path and at least one tag:

```bash
docker exec multiderp multiderp tailnet add work \
  --oauth-secret-file /data/secrets/work-oauth \
  --tag tag:multiderp
```

OAuth enrollment uses the secret file plus the configured tags. The secret value stays outside the YAML configuration.

### Auth key

```bash
docker exec multiderp multiderp tailnet add lab \
  --auth-key-file /data/secrets/lab-auth-key
```

The referenced file is part of the deployment's sensitive state and should have host permissions appropriate for credentials.

### Required verifiers

Add `--required` when the availability of a verifier should participate in service readiness:

```bash
docker exec multiderp multiderp tailnet add work \
  --oauth-secret-file /data/secrets/work-oauth \
  --tag tag:multiderp \
  --required
```

A disabled verifier is effectively optional until it is enabled again. `tailnet status --verbose` shows both configured and effective required state.

## Advertise the DERP server in each tailnet

Each tailnet controls its own DERP map through Tailscale policy. Add the custom DERP region in every tailnet that should discover this host, using the public hostname and ports from your deployment.

Tailscale's current custom-DERP documentation and policy syntax are maintained here:

<https://tailscale.com/docs/reference/derp-servers>

After updating policy, `tailscale netcheck` is useful for checking which DERP regions a client sees and can reach:

```bash
tailscale netcheck
```

The DERP map handles discovery. MultiDERP's admission layer independently decides whether the connecting node key belongs to one of the configured tailnets.

## Configuration

The configuration file is YAML schema version `1`. The container entrypoint uses:

```text
/data/config.yaml
```

### Server

| Field | Default | Purpose |
| --- | --- | --- |
| `server.hostname` | empty | Public DERP hostname. An enabled verifier requires a real hostname. |
| `server.derp.listen` | `:3377` | DERP TCP listener. |
| `server.derp.stun_listen` | `:3478` | STUN UDP listener. |
| `server.derp.tls_mode` | `external` | `external` or `passthrough`. |
| `server.derp.cert_mode` | `none` | `none`, `manual`, or `letsencrypt`, constrained by TLS mode. |
| `server.derp.cert_dir` | empty | Certificate directory for passthrough TLS. |
| `server.admin.socket` | `/run/multiderp/admin.sock` | Local admin Unix socket. |
| `server.health.listen` | `127.0.0.1:9090` | Health HTTP listener. |

When explicit hosts are used in both DERP and STUN listen addresses, the configuration validator expects the same host.

### Storage

| Field | Default | Purpose |
| --- | --- | --- |
| `storage.state_dir` | `/data` | Top-level application state. |
| `storage.tailnet_state_dir` | `/data/tailnets` | Per-verifier Tailscale state. |
| `storage.orphan_state_dir` | `/data/orphans` | State retained after verifier removal. |

### Logging

`logging.level` accepts:

```text
debug
info
warn
error
```

The default is `info`.

### Tailnet entries

A normalized verifier entry has this shape:

```yaml
tailnets:
  - name: personal
    disabled: false
    required: false
    hostname: multiderp-personal
    auth:
      type: web
      client_secret_file: ""
      auth_key_file: ""
      tags: []
```

`name` is MultiDERP's local verifier identifier. It is path-safe, at most 64 characters, and uses letters, digits, `-`, `_`, and `.`. When `hostname` is omitted, MultiDERP derives `multiderp-<name>`.

Authentication fields depend on `auth.type`:

| Type | Required material |
| --- | --- |
| `web` | Interactive login; secret files are not allowed. |
| `oauth` | `client_secret_file` plus one or more tags. |
| `auth_key` | `auth_key_file`. |

The CLI is the normal way to add and mutate verifier entries because it can coordinate configuration with verifier state.

## Admin CLI

The command surface is:

```text
multiderp version
multiderp serve [--config path] [--derper binary] [--admission-address address]

multiderp [--socket path] tailnet list
multiderp [--socket path] tailnet status <name> [--verbose]
multiderp [--socket path] tailnet add <name> [...]
multiderp [--socket path] tailnet enable <name>
multiderp [--socket path] tailnet disable <name>
multiderp [--socket path] tailnet login <name>
multiderp [--socket path] tailnet logout <name>
multiderp [--socket path] tailnet reset <name>
multiderp [--socket path] tailnet remove <name>

multiderp [--socket path] orphan list
multiderp [--socket path] orphan purge <orphan-id> [--yes]

multiderp [--socket path] config reload
multiderp [--socket path] derp restart
```

`serve` uses `127.0.0.1:3340` for the local admission callback by default. Use `--admission-address` only when the deployment topology requires a different address.

Typical container invocations:

```bash
docker exec multiderp multiderp tailnet list
docker exec multiderp multiderp tailnet status personal --verbose
docker exec multiderp multiderp config reload
```

### Enable and disable

```bash
docker exec multiderp multiderp tailnet disable personal
docker exec multiderp multiderp tailnet enable personal
```

Disabling removes the verifier from active admission while keeping its configuration and state available for later re-enable.

### Remove, inspect orphan state, and purge

```bash
docker exec multiderp multiderp tailnet remove personal
docker exec multiderp multiderp orphan list
```

Removal preserves the verifier state in the orphan-state area. Permanent deletion is a separate operation:

```bash
docker exec -it multiderp multiderp orphan purge <orphan-id>
```

`orphan purge` deletes retained verifier state. Use `--yes` only when automation has already made that destructive decision explicitly.

### Reload behavior

```bash
docker exec multiderp multiderp config reload
```

Reload supports ordinary reconciliations while protecting verifier identity. Authentication type, secret-file paths, tags, and verifier hostname are identity-sensitive fields for an existing verifier. Use the lifecycle commands when those fields need to move to a new identity/state relationship.

Likewise, verifier removal goes through `tailnet remove`, which gives the daemon a chance to preserve the old state as an orphan.

## Verifier states and admission

A verifier can report these states:

```text
configured
starting
waiting-login
hardening
connected
degraded
error
stopping
disabled
```

Admission eligibility is deliberately narrower than “the verifier process exists”:

```text
state == connected
and hardening_verified == true
```

Verbose status can include:

- authentication mode;
- configured/effective required state;
- current admission participation;
- login URL when present;
- resolved tailnet and node identity;
- node key and Tailscale IPs;
- state directory;
- last error.

### Hardening baseline

Before a verifier participates in admission, MultiDERP applies and reads back a minimum-capability Tailscale configuration. The current locked baseline includes:

- Shields Up enabled;
- remote configuration disabled;
- route-all disabled;
- zero exit-node selection;
- empty advertised routes and services;
- Tailscale SSH and web client disabled;
- Serve/Funnel/services empty;
- App Connector disabled;
- posture checking disabled;
- auto update disabled;
- drive shares empty;
- relay-server settings empty;
- backend running with a node key.

A drifted verifier leaves the admission pool before repair is attempted. A compatibility mismatch in the pinned LocalAPI contract moves the verifier to an error state so that a dependency/API mismatch is visible as such rather than being treated like an ordinary transient outage.

The full pinned compatibility matrix lives in [`HARDENING-COMPATIBILITY.md`](HARDENING-COMPATIBILITY.md).

### Admission limits

The current controller uses bounded work queues and timeouts:

| Limit | Current value |
| --- | ---: |
| Admission request timeout | 4 s |
| Per-verifier membership query timeout | 2 s |
| Concurrent admission requests | 64 |
| Concurrent verifier queries | 32 |
| Queued verifier jobs | 256 |

These are V1 implementation limits. Admission uses a snapshot of the current verifier pool and validates that an allowing verifier is still current before accepting the result.

## Health endpoints

The health listener exposes:

```text
/health/live
/health/ready
/health/startup
```

Each endpoint returns HTTP `200` when its condition is satisfied and `503` otherwise.

The health snapshot tracks information such as:

- process liveness;
- startup completion;
- DERP usability;
- eligible verifier count;
- required-verifier failures;
- pending restart state.

The default address is `127.0.0.1:9090`. In a container, that loopback address belongs to the container network namespace. Publish or rebind it deliberately if an external orchestrator needs direct access.

## Persistent state

`/data` can contain several different kinds of operational state:

```text
/data/config.yaml
/data/tailnets/...
/data/orphans/...
/data/certs/...        # direct-TLS deployments
/data/secrets/...      # if you choose this layout for credential files
```

The verifier directories contain Tailscale node identity and enrollment state, so backups of `/data` should be protected like the live host.

Verifier lifecycle commands preserve the relationship between configuration and state. `tailnet remove` moves old state to the orphan area; `orphan purge` is the explicit irreversible cleanup step.

## Security model

MultiDERP's operator controls the host, verifier state, configuration, and admission service. Host root or an equivalent administrator therefore sits inside the trust boundary.

DERP payload traffic remains protected by Tailscale's WireGuard encryption while it crosses the relay. The DERP host controls relay availability and holds verifier identities, but the relayed peer payload is still end-to-end encrypted by Tailscale.

The verifier identities are intentionally low-capability nodes. Their hardening state is verified before admission and re-checked over time. Tailnet owners can add Tailscale Grants/ACL policy around the dedicated verifier tag when they want an additional control-plane restriction.

The local administrative surfaces deserve the same boundary as the service state:

- `/run/multiderp/admin.sock` carries administrative authority;
- verifier state contains Tailscale node identity;
- OAuth/auth-key files contain enrollment credentials;
- the external-TLS backend listener carries plaintext DERP backend traffic.

Keep those surfaces on operator-controlled local or private paths.

STUN on UDP `3478` is endpoint-discovery infrastructure. DERP admission is still decided by the verifier callback path.

For vulnerability reports, see [`SECURITY.md`](SECURITY.md).

## Operational notes

### DERP is normally a fallback path

Tailscale attempts direct connectivity first. Current Tailscale clients can also use Peer Relays when configured, with DERP providing the broader fallback path. A custom DERP endpoint is therefore most useful when you specifically want control over relay placement or need better fallback locality for the participating tailnets.

Tailscale's current documentation also describes caveats around custom DERP and cross-tailnet sharing features. Keep deployment expectations aligned with the current upstream behavior:

<https://tailscale.com/docs/reference/derp-servers>

### STUN and DERP use different transports

The DERP endpoint is TCP/TLS; STUN is UDP `3478`. Test UDP reachability independently of the HTTPS relay path; STUN follows the host/network firewall path directly.

### Hostname changes are coordinated changes

`server.hostname` is the public name advertised in the tailnets' DERP maps. A hostname migration usually touches DNS, certificates, MultiDERP configuration, and each tailnet policy together.

## Troubleshooting

### The service is running but clients are rejected

Start with the verifier pool:

```bash
docker exec multiderp multiderp tailnet list
docker exec multiderp multiderp tailnet status <name> --verbose
```

A useful admission verifier is in `connected` state with hardening verified. `waiting-login`, `degraded`, `error`, and `disabled` states explain most “DERP process is up, admission still fails” cases.

### Web enrollment is waiting for login

```bash
docker exec multiderp multiderp tailnet login <name>
```

Complete the returned Tailscale login URL for the intended tailnet, then inspect verbose status again.

### Config reload rejects an identity change

Use the verifier lifecycle commands for authentication mode, secret-file, tag, hostname, and removal changes. Reload intentionally keeps an existing state directory tied to the verifier identity that created it.

### The public HTTPS endpoint works but STUN does not

Check UDP `3478` independently in the host firewall, cloud firewall/security group, and any NAT in front of the host.

### Clients never discover the custom region

The region is advertised from each tailnet's Tailscale policy. Check that policy first, then inspect the client view:

```bash
tailscale netcheck
```

### The container cannot write `/data`

The image runs as UID/GID `10001:10001`. Check bind-mount ownership and permissions on the Docker host.

### Health is unreachable from the Docker host

The default `127.0.0.1:9090` listener is inside the container namespace and is not published by the example Compose files. Expose it only when your monitoring topology requires that path.

## Build and test

The Go module is:

```text
github.com/lsy223622/MultiDERP
```

Build MultiDERP and the pinned upstream DERP binary:

```bash
go build ./cmd/multiderp
go build tailscale.com/cmd/derper
```

Run the usual checks:

```bash
go test ./...
go vet ./...
```

For concurrency-sensitive work:

```bash
go test -race ./...
```

The CI workflow also exercises supported builds and the container image.

## Tailscale dependency policy

The repository currently pins:

```text
tailscale.com v1.102.3
```

The Dockerfile builds `derper` from that same module version. A Tailscale upgrade therefore changes both the relay binary and the verifier APIs that underpin hardening/admission.

Review [`HARDENING-COMPATIBILITY.md`](HARDENING-COMPATIBILITY.md) and rerun the repository's relevant unit tests, race tests, builds, and container build as part of any dependency bump.

## Security reports

Use GitHub's private vulnerability-reporting path for security issues:

<https://github.com/lsy223622/MultiDERP/security/advisories/new>

Credential values, private node keys, verifier state, and certificate private keys belong in the private report rather than a public issue.

## License

MultiDERP is licensed under the [GNU General Public License v3.0](LICENSE).

The built image also contains upstream Tailscale code. Review the repository's third-party license material when redistributing binaries or images.
