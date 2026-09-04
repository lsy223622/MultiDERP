# Security policy

## Reporting a vulnerability

Please report suspected vulnerabilities privately through a GitHub Security
Advisory for this repository:

<https://github.com/lsy223622/MultiDERP/security/advisories/new>

Do not include credentials, private node keys, Tailnet state, or other secrets
in a public issue. If private advisories are unavailable, open a minimal public
issue asking for a private reporting channel without disclosing exploit
details.

## Trust model and security boundaries

MultiDERP is an operator-controlled DERP relay and admission service. The
operator is trusted with the verifier state directories, which contain
Tailscale node identities and other local enrollment state. The threat model
does not treat a malicious root or equivalent host administrator as an
attacker; such an administrator can inspect or modify the service and its
state.

The verifier process is hardened before it can admit clients: it enables
ShieldsUp, keeps routes, services, SSH, Web, Serve/Funnel, remote configuration,
and related capabilities disabled, and periodically reads back the relevant
LocalAPI state. A detected drift removes the verifier from admission before a
repair is attempted.

DERP relay traffic remains protected by Tailscale's WireGuard encryption. The
STUN listener is public network infrastructure for endpoint discovery; STUN
reachability does not grant Tailnet admission and does not replace the
admission callback. Keep the admin socket, verifier state, secret files, and
plaintext backend listener on protected local or private networks.

Tailnet owners who need stronger control-plane isolation should apply their own
Tailscale Grants or reviewed ACL policy to the dedicated verifier tag and must
verify that the policy still permits the control-plane lookups MultiDERP uses.
