# Hardening compatibility matrix

This matrix records the public APIs used to keep a verifier at the minimum
capability baseline. The locked dependency for this repository is
`tailscale.com v1.102.3`; the `derper` image binary is built from the same Go
module in the Dockerfile. A dependency upgrade requires this matrix and the
release integration tests to be reviewed again.

| Invariant | Apply operation | Read-back check | API maturity in v1.102.3 | Unavailable handling | Admission gate |
| --- | --- | --- | --- | --- | --- |
| `ShieldsUp=true` | `local.Client.EditPrefs` with `ShieldsUpSet` | `GetPrefs` | stable | compatibility error | required |
| `RemoteConfig=false` | `EditPrefs` with `RemoteConfigSet` | `GetPrefs` | stable | compatibility error | required |
| `RouteAll=false` | `EditPrefs` with `RouteAllSet` | `GetPrefs` | stable | compatibility error | required |
| No exit node | `EditPrefs` with zero `ExitNodeID`, invalid `ExitNodeIP`, empty `AutoExitNode`, and false LAN access | `GetPrefs` | stable | compatibility error | required |
| No advertised routes/services | `EditPrefs` with empty values and masks | `GetPrefs` | stable | compatibility error | required |
| No SSH or web client | `EditPrefs` with `RunSSHSet` and `RunWebClientSet` | `GetPrefs` | stable | compatibility error | required |
| App Connector disabled | `EditPrefs` with `AppConnectorSet` | `GetPrefs` | stable | compatibility error | required |
| Posture checking disabled | `EditPrefs` with `PostureCheckingSet` | `GetPrefs` | stable field in locked release | compatibility error | required |
| Auto update disabled | `EditPrefs` with both `AutoUpdate` mask bits | `GetPrefs` | stable field in locked release | compatibility error | required |
| Running and not logged out | `EditPrefs` with `WantRunning=true` and `LoggedOut=false` | `GetPrefs` plus `Status` must report `Running` and a node key | stable `GetPrefs`/`Status` | compatibility error | required |
| Serve/Funnel/Services empty | `SetServeConfig(nil)` | `GetServeConfig` must have no TCP, web, service, funnel, or foreground entries | locked-release public API; no explicit stable mark | compatibility error | required |
| Drive shares empty | `EditPrefs` with `DriveSharesSet=true` and `DriveShares=nil` | `GetPrefs` must report no `DriveShares` | stable `Prefs` field plus stable `EditPrefs`/`GetPrefs` in locked release | compatibility error | required |
| Relay server disabled | `EditPrefs` with nil relay port/endpoints and masks | `GetPrefs` | stable field in locked release | compatibility error | required |

`WhoIsNodeKey` is the only verifier lookup used by admission. It is called
through the `Verifier` interface after the verifier has passed every gate in
this table. The daemon does not expose `*tsnet.Server` outside the
`internal/verifier/tsnet` package, and that package does not call `Dial`,
`Listen`, `Loopback`, `ListenFunnel`, or other application-networking APIs.

The upstream `ipnstate.Status` exposes `BackendState`, `HaveNodeKey`, `AuthURL`, tailnet identity, and
assigned Tailscale IPs. `ipnstate.Status` in this locked release does not
provide a separate `LoggedOut` field; the running state plus node-key check,
combined with the explicit prefs baseline, is the read-back used here.

The matrix is intentionally fail-closed. If a future locked release removes
one of these public read-back operations, the verifier must stay out of the
admission pool until a replacement operation is identified and tested.

The implementation recognizes unsupported LocalAPI responses (404, 405, 501,
or an explicit not-implemented/unsupported error) while applying this matrix
as `ErrHardeningCompatibility`. Such a verifier enters `Error`, is removed
from the admission pool, and is not automatically retried. A later explicit
configuration reconcile/enable is the retry boundary. Ordinary transport,
timeout, and transient LocalAPI failures remain `Degraded` with bounded
backoff. This prevents a locked-release API mismatch from being hidden as a
temporary outage while preserving recovery for a temporarily unavailable
daemon.
