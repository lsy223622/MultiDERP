# Security

## Reporting

Report suspected vulnerabilities privately through this repository's GitHub Security Advisory facility. If private reporting is unavailable, open a minimal issue requesting a private channel without exploit details. Do not disclose OAuth secrets, passwords, node private keys, enrollment codes, device-key inventories or database backups in public issues.

## Trust boundaries

The platform administrator, controller host administrator and relay provider are trusted. The controller stores OAuth credentials encrypted with its local key, manages accounts and grants, and supplies effective device-key policies. A provider operates the process that enforces its relay policy and can observe relay metadata and traffic volumes. A host administrator can inspect or replace code, keys, databases and clocks. Local audit records do not provide tamper resistance against that administrator.

Tailscale's WireGuard encryption protects relayed peer payloads. Sharing a DERP endpoint does not merge tailnets or override their ACLs/grants. UniDERP requires no additional Tailscale node membership for the relay hosts. Standard clients prove possession of the admitted node private key through the DERP protocol; knowing a permitted public key alone is insufficient.

## Credentials and authentication

Configure a dedicated OAuth client with `devices:core:read` only. UniDERP requests that scope and uses device reads; this does not prove the original client has no broader permissions. OAuth ID/secret remain on the controller, encrypted at rest. Protect its database and `controller.key` together; encryption does not protect against a host administrator with both files.

Platform passwords are independent of OAuth and stored as password hashes. Sessions use protected cookies, and account mutations require CSRF validation and server-side authorization. Member nodes use their persistent Ed25519 identity and scoped node sessions; a node session cannot call platform account APIs. Enrollment codes are single-use, time-limited and bound to a resource, private-key proof and verified HTTPS domain.

Keep admin sockets, management backends, health listeners and data directories private. The external HTTP backend must only be reachable by the TLS proxy. Public TLS certificates must validate normally. Domain probes reject redirects and unsafe destinations unless explicitly allowed by controller deployment configuration. STUN is public discovery infrastructure and is separate from DERP admission.

## Authorization and time

Third-party sharing requires a request, provider approval and applicant confirmation. A provider's own tailnet can become active directly. Exported maps advertise resources; they do not authorize device keys. The controller's local relay follows the same resource rules as member relays.

Effective permission ends at the earliest identity-retention, controller-contact, explicit-grant or device-key deadline. Only complete successful identity refreshes extend identity validity. Failed calls, partial results and process restarts do not reset existing deadlines. The controller exposes configurable identity/control retention; the initial values are 24 hours each.

Online revocation takes effect when derper applies the new policy, including closure of existing connections. Receipt alone is not application. Offline nodes retain their last effective policy only until its original absolute deadline. Cache revision/digest watermarks reject inconsistent or rolled-back snapshots; they are not protection against root rewriting the entire persistent state.

## Deployment and recovery

Run the container as UID/GID 10001 with a read-only root filesystem, writable protected data volume and private runtime tmpfs. Direct TLS may require `NET_BIND_SERVICE`; the service does not need privileged mode or network-administration capability. Use protected files for bootstrap passwords and enrollment codes, and delete the temporary files after use.

Back up the controller SQLite database consistently, including WAL when appropriate, and preserve its matching encryption key and the full node/derper identity state. Stop duplicate instances before identity recovery. Missing keys or corrupted caches require diagnosis and a consistent restore; deleting state is not an authorization repair.

## Validation limits

The project includes direct API authorization/CSRF/SSRF/replay tests, scoped management workflows, DERP library proof and revocation tests, bounded byte-queue tests, local TLS/STUN checks and Linux race execution. Separate isolated public-network acceptance exercised two independent real tailnets with dedicated read-only OAuth and stock Tailscale clients using normally distributed standard DERP maps. File hashes and per-tailnet relay counters verified controller/member forwarding through external TLS and manual-certificate passthrough; pre-confirmation denial and online revocation/recovery were also exercised. The complete canonical Dockerfile build passed with normal caching, with nonroot persistence, restart, shutdown and control-failure tests recorded separately.

Automatic certificate issuance remains unverified. Earlier builds without cached dependencies failed on dependency-download EOF errors. These results do not establish long-term WAN throughput or latency, availability, or protection from a malicious trusted host operator. No v2 release has been published.
