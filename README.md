# MultiDERP

MultiDERP is a small admission-control layer around the upstream Tailscale
derper. One daemon can run independent tsnet.Server verifiers for several
Tailnets while the DERP datapath remains the upstream child process. A client
is admitted only when at least one eligible verifier confirms its NodePublic
with WhoIsNodeKey.

The V1 boundary is deliberately narrow:

- Tailscale's official control plane only; control_url is rejected.
- One isolated state directory and tsnet identity per verifier.
- ShieldsUp, no routes/services/SSH/Web/Serve/Funnel, disabled
  Remote Config, and a read-back hardening check before admission.
- No DERP mesh, no local tailscaled client verification, no quotas, and no
  custom DERP datapath.
- YAML and secret files are the configuration surface. Secret values are never
  written to YAML or returned by the admin API.

## Quick start

The example configuration contains only placeholders. Replace the public
hostname and provide secret files through your deployment's protected secret
mechanism.

The Compose examples use the public release image
`ghcr.io/lsy223622/multiderp:latest`. Release tags also publish a version tag
such as `ghcr.io/lsy223622/multiderp:1.0.1`; use that tag or the image digest
when a deployment must not follow later releases automatically.
The `latest` tag is updated only by a stable `vX.Y.Z` release; a running
container still needs an image updater or a scheduled `docker compose pull`
and `docker compose up -d` to apply that update.

Linux/macOS:

~~~sh
mkdir -p data
cp config.example.yaml data/config.yaml
docker compose -f docker-compose.example.yaml up -d
~~~

PowerShell:

~~~powershell
New-Item -ItemType Directory -Force data
Copy-Item config.example.yaml data\config.yaml
docker compose -f docker-compose.example.yaml up -d
~~~

The example intentionally starts with tailnets: []. In that state the manager,
admin socket, and health server can run, but no derper child is started and
readiness remains false. Add the first verifier through the Unix admin socket:

~~~text
docker exec multiderp multiderp tailnet add alice
docker exec multiderp multiderp tailnet list
docker exec multiderp multiderp tailnet status alice
~~~

For non-interactive enrollment, point to a read-only secret file:

~~~text
docker exec multiderp multiderp tailnet add company --oauth-secret-file /run/secrets/company-oauth --tag tag:multiderp-verifier
docker exec multiderp multiderp tailnet add lab --auth-key-file /run/secrets/lab-auth-key
~~~

OAuth enrollment requires at least one advertised tag. Repeat `--tag` for
multiple tags; the order is preserved in the admin request and in the
verifier configuration. The equivalent YAML entry is:

~~~yaml
auth:
  type: oauth
  client_secret_file: /run/secrets/company-oauth
  tags:
    - tag:multiderp-verifier
~~~

Web authentication returns a Tailscale login URL. The URL means that login is
required; it does not mean that the verifier is already eligible. After login,
MultiDERP applies and reads back the hardening baseline again before publishing
the verifier to admission.

## Configuration and lifecycle

/data/config.yaml is required at daemon startup. A missing file is an error
and exits before the socket or derper child is created. An existing empty,
null, comment-only, or {} YAML file is an intentional empty desired config.

The daemon is the authoritative writer. Runtime changes go through the admin
socket, are validated, written with a temporary file plus fsync and atomic
rename, and only then reconciled. Hand-editing is supported through:

~~~text
docker exec multiderp multiderp config reload
~~~

Invalid reloads leave the active runtime unchanged. Listener, TLS/certificate,
admin, health, storage-root, and server-hostname changes are persisted as
pending restart changes; the current listeners, child, and state root stay in
use until the daemon is restarted.
Verifier identity/auth/hostname changes are rejected while reusing state;
perform an explicit reset or remove-and-add operation instead.
Removing a verifier by hand from YAML is also rejected during reload; use
`tailnet remove <name>` so its state is moved to a generated orphan directory.

Useful operations:

~~~text
multiderp version
multiderp tailnet enable <name>
multiderp tailnet disable <name>
multiderp tailnet login <name>
multiderp tailnet logout <name>
multiderp tailnet reset <name>
multiderp tailnet status <name> --verbose
multiderp tailnet remove <name>
multiderp orphan list
multiderp orphan purge <orphan-id> --yes
multiderp derp restart
~~~

disable, logout, reset, and remove remove the verifier from admission before
closing it. reset requires a successful LocalAPI logout before local state is
deleted. remove preserves the state under a generated orphan ID; only an
explicitly confirmed orphan purge deletes it. A same-name add never loads an
orphan automatically.

derp restart is the global session-revocation operation. It temporarily denies
new admission, stops the child, and starts it again with the same persistent
DERP key and current eligible verifier pool. An unexpected child exit makes
the parent exit non-zero so the container supervisor can restart the complete
service.

`tailnet status --verbose` includes the full verifier NodeKey for operator
diagnostics; ordinary status output redacts it.

## TLS, public endpoint, and backend listener

MultiDERP separates the public DERP endpoint from the listener used by the
local derper process. `HostName` and `DERPPort` in each Tailnet's DERPMap are
the public endpoint that clients use; `server.derp.listen` is only the
listener reached by a local reverse proxy or by the client when derper itself
terminates TLS. Never copy an internal backend port into a public DERPMap by
accident.

For example, the Nginx deployment below has this topology:

~~~text
DERPMap -> https://derp.example.com:443
Nginx   -> http://127.0.0.1:3377
~~~

The default backend port is TCP `3377` and is internal to the deployment; it
must not be opened to the public Internet. STUN is UDP `3478`; the Compose
examples publish it explicitly on both IPv4 and IPv6, directly or through a
proxy that supports UDP. The host and Docker daemon must have IPv6 enabled for
the IPv6 mapping to work. The public DERP endpoint can still be TCP `443`; it
is independent of the backend port.

V1 supports exactly these certificate modes:

- `cert_mode: none` with `tls_mode: external`: derper provides the HTTP/DERP
  backend and a compatible reverse proxy terminates public TLS.
- `cert_mode: manual` with `tls_mode: passthrough`: derper uses the supplied
  certificates and may listen on any configured TCP port.
- `cert_mode: letsencrypt` with `tls_mode: passthrough`: derper obtains and
  renews its own certificates and must listen on port 443.

`cert_mode: gcp` is not supported in V1 and is rejected as an unsupported
configuration value. Certificate settings are intentionally not mixed between
the external and passthrough listener models.

### A. MultiDERP terminates Let's Encrypt on 443

Use this model when derper should provide HTTPS directly. Persist the
certificate directory and publish the resulting public endpoint in the
Tailnet's DERPMap:

~~~yaml
server:
  hostname: derp.example.com
  derp:
    listen: ":443"
    stun_listen: ":3478"
    tls_mode: passthrough
    cert_mode: letsencrypt
    cert_dir: /data/derper-certs
~~~

The DERPMap entry is `HostName: derp.example.com`, `DERPPort: 443`, and
`STUNPort: 3478`. There is no separate public backend listener in this model.
The upstream HTTP-01 challenge listener is enabled on port 80, so public TCP
80 must reach the same container during issuance and renewal. The example
container runs as UID/GID 10001; use the
`docker-compose.letsencrypt.example.yaml` profile, which grants only
`NET_BIND_SERVICE` so the process can bind 80 and 443. It does not use
`CAP_NET_ADMIN` or privileged mode. Persist `/data` so the ACME state and DERP
key survive restarts.

### B. Nginx terminates public TLS on 443

Use this model when Nginx owns the public certificate and MultiDERP is an
internal HTTP/DERP backend:

~~~yaml
server:
  hostname: derp.example.com
  derp:
    listen: ":3377"
    stun_listen: ":3478"
    tls_mode: external
    cert_mode: none
~~~

The DERPMap still contains only `derp.example.com:443`; clients do not know
that Nginx forwards to the internal backend. The default Compose example binds
that backend to `127.0.0.1:3377` for a host Nginx. If Nginx runs as a container
on the same private network, use `multiderp:3377` instead. Keep 3377 on the
private container network or bind it only to a protected local interface. Do not
publish it as a second public DERP endpoint.

If a deployment explicitly needs another backend port, `:8443` is a valid
custom choice, but it is not the default and every proxy upstream and port
mapping must be changed consistently:

~~~yaml
server:
  derp:
    listen: ":8443" # explicit custom backend port
~~~

DERP is a long-lived bidirectional upgrade stream. Nginx must support
HTTP/1.1, preserve Upgrade and Connection, disable buffering for /derp, and
use long read/write timeouts. A browser response or ordinary HTTP 200 is not a
DERP relay test.

The public HTTP surface should be an allowlist. For Nginx, adapt the public
certificate configuration to your deployment:

~~~nginx
# Put this map in the http {} context.
map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      close;
}

upstream multiderp_backend {
    server multiderp:3377;
}

server {
    listen 443 ssl;
    server_name derp.example.com;

    # Public certificate configuration belongs to this proxy.

    location = /derp {
        proxy_pass http://multiderp_backend;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        proxy_set_header Host $host;
        proxy_buffering off;
        proxy_request_buffering off;
        proxy_read_timeout 24h;
        proxy_send_timeout 24h;
    }

    location = /derp/probe         { proxy_pass http://multiderp_backend; }
    location = /derp/latency-check { proxy_pass http://multiderp_backend; }
    location = /bootstrap-dns      { proxy_pass http://multiderp_backend; }
    location = /generate_204       { proxy_pass http://multiderp_backend; }

    location / { return 404; }
}
~~~

Do not publish /debug, /metrics, the admin socket, or arbitrary upstream
paths. If the chosen proxy cannot carry the DERP upgrade stream, use
tls_mode: passthrough with an L4 TCP forwarder and choose `manual` or
`letsencrypt` according to the certificate model above. The DERP map is
configured in each Tailnet's control plane; MultiDERP does not create, rewrite,
or remove DERP map regions.

## Health and security

The health listener defaults to loopback and exposes:

~~~text
GET /health/live
GET /health/ready
GET /health/startup
~~~

Successful checks return JSON with HTTP 200; failed checks return the same
shape with HTTP 503. Readiness requires a usable child, at least one eligible
verifier, and every enabled required verifier to be eligible. A disabled
required verifier does not count as a failure. Health and admission endpoints
are separate from the public DERP surface.

The container runs as UID/GID 10001, with a read-only root filesystem, no TUN
device, and no CAP_NET_ADMIN. Only /data and the owner-controlled
/run/multiderp tmpfs need to be writable. Keep verifier state and secret files
protected: state contains a Tailnet node identity, and the server operator is
therefore trusted with that identity. Tailnet owners who need stronger
isolation should additionally apply their own Tailscale Grants/ACL policy.
The daemon writes `.multiderp-remove-operation.yaml` beside the configured
config file while a remove operation is in progress. It is a private,
non-secret recovery journal. On startup, the journal is completed or rejected
before any verifier or listener is started; do not delete it to bypass a
recovery error.

Server-side hardening only limits the capabilities of the verifier process and
requires the DERP operator to protect its state directory. Tailnet owners who
also need control-plane-enforced isolation should use their own Tailscale
Grants (or an explicitly reviewed legacy ACL) with a dedicated verifier tag,
such as `tag:multiderp-verifier`. Verify both that this tag cannot initiate
application access to other devices and that the control plane still exposes
the nodes that `WhoIsNodeKey` must recognize. Lack of application access does
not necessarily mean that a node is invisible to control-plane lookups.
MultiDERP does not parse, create, upload, or reload the Tailnet policy.

## Development

The pinned upstream dependency is tailscale.com v1.102.3; the Dockerfile
builds multiderp and the upstream derper from that same module version. Do not
upgrade it independently of the hardening compatibility matrix and the
release integration tests.

With Go's automatic toolchain selection enabled:

~~~text
multiderp version
go test ./...
go build ./cmd/multiderp
go build tailscale.com/cmd/derper
~~~

The CI/test boundary uses fake verifiers and does not require real Tailscale
accounts. Real web/OAuth/auth-key enrollment, public DERP upgrade behavior,
reverse-proxy forwarding, and UDP STUN reachability remain controlled release
tests in the target deployment.

GitHub Actions runs independent checks on every push and pull request:

- Go unit tests and `go vet` on Linux;
- Linux race detection with `go test -race ./...`;
- CGO-disabled Linux amd64 builds of MultiDERP and the pinned upstream derper;
- a CGO-disabled Windows amd64 build of both binaries;
- the Docker image build from the digest-pinned inputs in
  [release-manifest.yaml](release-manifest.yaml).

The manifest records the exact `tailscale.com` commit and Linux image digests;
the module version remains pinned in `go.mod` and must be reviewed together
with [HARDENING-COMPATIBILITY.md](HARDENING-COMPATIBILITY.md) before any
upgrade.
