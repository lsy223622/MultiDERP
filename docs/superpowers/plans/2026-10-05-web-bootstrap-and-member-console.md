# Web 首次设置与成员节点管理 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 部署者在网页完成角色选择、本机中继注册、成员入群／退群、本机配置与本地总限速调整；主要限速和优先级规则仍由资源所有者在主控管理。

**Architecture:** 保留现有 Store、账号会话、注册证明、策略控制流和 derper 子进程。管理 HTTP 服务先启动，主控身份同步与成员控制连接按持久配置单独启停；通过一个本机管理接口连接 HTTP handler 与 Daemon。HTTPS 反代将管理与集群路径转发给独立管理入口，DERP 数据路径继续连接既有 derper，避免退群或中继重启使网页不可用。

**Tech Stack:** Go 1.27.1、net/http、SQLite、现有 YAML 配置、原生 JavaScript、Docker Compose、nginx；不新增依赖。

**Spec:** `docs/superpowers/specs/2026-10-05-web-bootstrap-and-member-console-design.md`

## Global Constraints

- 界面统一称 **主控**、**成员节点**、**集群**、**Tailnet**；OAuth 提示只写 `devices:core:read`。
- 成员节点使用本机独立的管理员账号，与主控失联时仍能登录和管理。
- 密码、注册码和私钥不进入 URL、日志或普通配置读取响应。
- 本机配置、注册、退群和服务重启只允许本机管理员调用。
- 不扩大 DERP 数据协议、设备许可或跨 Tailnet 共享权限；带宽更新不延长授权期限。
- 本地总限速不被主控下发覆盖；主控预算超出本地限速仍接收／应用策略，以较小值作为调度总预算。
- 已有 YAML 和本地命令继续有效；现有主控数据库、节点私钥和注册记录原地保留。
- 当前服务器的主控数据原地升级；成员流程使用独立测试实例，保留生产 multiderp 条目。

## Review Focus

1. 空域名、未选角色或尚未入群：HTTPS 网页仍可用，设备不得获得中继许可（Task 1、2、7）。
2. 监听端口被占用或证书不匹配：不能报告应用成功，已保存／正在运行的配置必须可区分，管理员仍能修正（Task 4）。
3. 主控失联或退群中途重启：本机退出结果持久化，旧会话／缓存不能重新放行设备（Task 3、5）。
4. 其他服务器的 Cookie、远端节点 Token 或普通平台账号：不能调用本机配置接口；节点 Token 只能操作自己的退出及公开端口，主控账号按资源权限操作主要规则（Task 2、6）。
5. 本地限速低于下发预算且存在活跃共享授权：应用 ACK 成功、调度受本地限制，权重、设备清单、授权期限及原策略摘要保持原值（Task 6、7）。

## Files and interfaces

新增文件围绕本机管理与节点自管理划分，不重排其他业务代码。

- `internal/config/bootstrap.go`：首次设置默认配置；其余 YAML 校验仍在 `config.go`。
- `internal/control/local_http.go`：本机管理 DTO、`LocalBackend` 和管理员 HTTP 路由。
- `internal/daemon/management.go`：管理入口与本机状态；`local_config.go`：保存／应用配置与证书；`local_cluster.go`：本机注册、入群／退群及节点自管理请求。
- `internal/control/node_self.go`：使用节点会话的退出、公开端口、带宽接口；对应事务逻辑复用 `nodes.go`、`qos.go`、`resources.go`。
- `internal/control/ui/app.js`、`index.html`、`app.css`：沿用现有页面和表单，不引入前端框架。

`LocalBackend` 位于 control 包，Daemon 实现下列方法；HTTP 层完成账号与 CSRF 验证，Daemon 串行执行状态操作：

```go
Status(context.Context) (LocalStatus, error)
SaveSettings(context.Context, LocalSettings) error
ApplySettings(context.Context) error
UploadCertificate(context.Context, string, string) error
Join(context.Context, string, string) error // controller HTTPS origin, enrollment code
Leave(context.Context) (bool, error)        // bool: controller confirmed release
RegisterLocal(context.Context, Actor, string) (Node, error) // resource display name
```

`LocalSettings` 包含 `Role string`、`Hostname string`、`DERPListen string`、`STUNListen string`、`TLSMode string`、`CertMode string`、`DERPPort int`、`STUNPort int`、`MaxBudgetBPS uint64`、`LoggingLevel string`。证书目录、数据库、管理监听、socket 和数据路径不接受网页修改。`Role` 只允许初始状态选择 `controller` 或 `member`。

`LocalStatus` 包含 `Role string`（`setup/controller/member`）、`Joined bool`、`ControllerURL string`、`ClusterID string`、`NodeID string`、`Saved LocalSettings`、`Active LocalSettings`、`PendingApply bool`、`ApplyError string`、`Control cluster.ControlStatus`、`PolicyBudgetBPS uint64`（已接收的主控预算）、`EffectiveBudgetBPS uint64`（derper 当前调度预算）。不返回 Session Token、证书私钥或原始注册请求。

HTTP 路径固定为 `/api/v1/local/status`、`/settings`、`/apply`、`/certificate`、`/join`、`/leave`、`/register`。所有路径都置于 `/api/v1/local` 下，只有 status 支持 GET，其余写操作用 POST；settings 用 POST 保存，本地限速通过其中的 MaxBudgetBPS 设置。

### Task 1: 持久首次设置状态与未入群配置

**Files:** Create `internal/config/bootstrap.go`, `internal/config/bootstrap_test.go`; modify `internal/config/config.go`, `internal/config/cluster_test.go`, `internal/daemon/daemon.go`。

**Interfaces:** `config.BootstrapYAML() []byte` 生成首次运行模板；新增 `Config.SetupRequired bool`（`setup_required,omitempty`）、`Server.Management ManagementConfig`（`Listen string`）、`Node.DERPPort int`、`Node.STUNPort int`。

- [ ] 写 `TestBootstrapConfigRequiresRoleAndHasManagementEntry`，断言 Parse(BootstrapYAML()) 成功、SetupRequired 为 true、Hostname 为空、Management.Listen 为 `:3378`，DERP／STUN 默认值仍为 `:3377`／`:3478`。
- [ ] 写 `TestExplicitRolesAndUnjoinedMemberConfig`，断言原主控／成员 YAML 没有 setup_required 时不进入向导；未入群成员允许空 ControllerURL，非空时仍必须是 HTTPS origin；公开端口默认 443／3478，拒绝 0 以下和 65535 以上的显式值。
  ```go
  parsed, err := config.Parse(config.BootstrapYAML())
  if err != nil || !parsed.Config.SetupRequired || parsed.Config.Server.Hostname != "" || parsed.Config.Server.Management.Listen != ":3378" { t.Fatal(parsed, err) }
  if parsed.Config.Node.DERPPort != 443 || parsed.Config.Node.STUNPort != 3478 { t.Fatal(parsed.Config.Node) }
  ```
- [ ] 运行 `go test ./internal/config -run 'Bootstrap|ExplicitRoles|Unjoined' -count=1`，确认新行为尚未实现而失败。
- [ ] 更新 schema、Normalize、Validate、Clone 与 RestartOnlyChanged；缺失配置采用 BootstrapYAML，明确传入 Options.ConfigTemplate 和现有文件继续按现有配置启动。空成员 URL 只表示未入群，不放宽已注册绑定检查。
- [ ] 重跑上述测试及 `go test ./internal/config -count=1`；预期全部 PASS、现有字段校验保持有效。提交 `feat: add persistent web bootstrap configuration`。

### Task 2: 本机管理员 HTTP 边界与独立管理入口

**Files:** Create `internal/control/local_http.go`, `internal/control/local_http_test.go`, `internal/daemon/management.go`, `internal/daemon/management_test.go`; modify `internal/control/http.go`, `internal/daemon/controller.go`, `internal/daemon/node.go`, `internal/daemon/daemon.go`。

**Interfaces:** 定义上面的 DTO 和 LocalBackend；增加 `control.NewLocalHTTPHandler(*Store, LocalBackend) http.Handler`，与现有 NewHTTPHandler 共用认证和页面处理。`Daemon.startManagement(context.Context, config.Config) error` 先于角色业务启动。

- [ ] 写 `TestLocalManagementRequiresLocalAdminAndCSRF`：用已有 accountRequest/loginTest 方法，断言匿名 401、普通平台账号 403、管理员缺 CSRF 403；另一 Store 的 Cookie、节点 Bearer Token 均不能调用本机写接口。使用 fakeLocalBackend 记录操作次数，所有拒绝请求计数必须为 0。
- [ ] 写 `TestMemberManagementDoesNotExposeControllerAPIs`：成员管理员登录、session、修改自己的密码和本机状态成功；Tailnet、OAuth、grants、users 列表及主控 cluster 接口返回 404；domain challenge 仍交给当前 EnrollmentClient。
- [ ] 写 `TestManagementSurvivesMissingHostnameAndStoppedRelay`：空域名／初始实例与已停止 derper 均可通过 Management.Listen 加载页面、设置管理员和登录；初始实例没有主控身份同步和成员控制流。
  ```go
  if anonymous.Code != 401 || ordinaryUser.Code != 403 || missingCSRF.Code != 403 || backendCalls != 0 { t.Fatal("local authority boundary") }
  if managementPage.Code != 200 || login.Code != 200 || controllerAPI.Code != 404 { t.Fatal("member management surface") }
  ```
- [ ] 运行 `go test ./internal/control ./internal/daemon -run 'LocalManagement|MemberManagement|ManagementSurvives' -count=1`，确认失败。
- [ ] 从现有 handler 中提取共用构造过程，不复制密码、Cookie 或 CSRF 算法；增加动态角色路由限制。session 响应增加 `server_role`，用于登录后的页面分流。保留原 NewHTTPHandler 用于现有主控测试，生产 Daemon 使用 NewLocalHTTPHandler。
- [ ] 管理 loopback 入口保留主控的 Controller.Listen、成员的 `127.0.0.1:3341`；额外的 Server.Management.Listen 使用同一 handler，接收 HTTPS 反代的请求。默认旧配置不增加对外监听。成员账号 Store 使用 `<storage.state_dir>/controller.sqlite`，只启用账号功能；主控使用原 Controller.Database，首次选择主控继续复用初始 Store。主控身份密钥仅在配置为主控后启用。
- [ ] 管理服务在 Daemon Shutdown 时关闭；角色变化、控制失联、身份冲突、中继停止不关闭管理服务。业务启动／应用错误记录到本机状态，保留管理入口让管理员修正；管理监听和 Store 自身故障仍作为启动失败处理。
- [ ] 重跑针对性测试，预期全部 PASS；提交 `feat: expose independent local server management`。

### Task 3: 节点退出、主控释放与原私钥重新注册

**Files:** Create `internal/control/node_self.go`, `internal/control/node_leave_test.go`; modify `internal/control/nodes.go`, `internal/control/node_http.go`, `internal/control/http.go`, `internal/control/control_stream.go`, `internal/cluster/node_client.go`, `internal/cluster/node_client_test.go`, `internal/cluster/cache.go`, `internal/cluster/cache_test.go`。

**Interfaces:** `Store.ReleaseNode(ctx context.Context, token string) error`；`EnrollmentClient.Release(ctx context.Context, previous NodeSession) error` 调用 `POST /cluster/v1/node/leave`；`EnrollmentClient.ClearRegistration() (NodeSession, error)` 持久清空后返回仅在本次操作内使用的旧会话；`cluster.RemoveCache(path string) error` 删除指定策略与其 `.watermark`；`EnrollmentClient.SetControllerURL(string) error` 仅允许未注册实例切换地址。

- [ ] 写 `TestNodeReleaseRevokesSessionGrantsAndReceipt`：注册后退出，旧 Token、renew challenge 和旧 enrollment receipt 均被拒绝，资源 state 为 pending，原 public_key 保留，既有授权撤销且版本增加，控制流关闭。
- [ ] 写 `TestReenrollmentRequiresOriginalKeyAndNewConsent`：资源所有者重新签发注册码后，原私钥和域名证明成功，其他私钥失败；新策略版本大于旧版本，但旧 grants 不恢复。使用已有 nodeTestStore、grantFixture 和注册测试中的真实 Ed25519 签名方式。
- [ ] 写 `TestClearRegistrationPersistsBeforeCacheCleanup`：清空注册后重新构造客户端，PolicyBinding 两个值均为空，node.key 不变；RemoveCache 清除 policy.json 与 watermark，缺失文件不报错。清理失败仍保持持久未注册状态。
  ```go
  if released.State != "pending" || !bytes.Equal(publicKeyReadFromDatabase, originalPublicKey) { t.Fatal(released) }
  if _, err := store.AuthenticateNode(t.Context(), oldToken); !errors.Is(err, control.ErrUnauthorized) { t.Fatal(err) }
  if clusterID, nodeID := reopened.PolicyBinding(); clusterID != "" || nodeID != "" { t.Fatal(clusterID, nodeID) }
  ```
- [ ] 运行 `go test ./internal/control ./internal/cluster -run 'NodeRelease|Reenrollment|ClearRegistration|RemoveCache' -count=1`，确认失败。
- [ ] 释放事务按 nodeSession 验证 Token 的所属节点与实例，撤销会话、旧注册码／receipt／challenge、观测和既有授权，清空 lease 与当前实例，保留资源、原私钥及策略版本序列；提交后关闭对应控制流。IssueEnrollment 对所有者明确的重新注册操作复用同一撤销事务。
- [ ] EnrollmentChallenge 与 EnrollNode 接受 pending 且 public_key 为空或与请求原私钥相同的资源；仍需一次性新注册码与 HTTPS 域名验证。保留 node_policies 行及递增版本，避免重置策略版本。
- [ ] ClearRegistration 先原子保存没有 session/pending 的 registration.json，再允许调用者清理缓存；返回的旧会话仅供本次 Release 调用，不写回注册记录、不返回 HTTP 响应。SetControllerURL 继续使用原 HTTPS origin 校验，禁止已注册节点换主控。客户端没有注册时可使用空 URL，仅提供本机身份／域名验证，不发送控制请求。
- [ ] 重跑针对性测试，预期全部 PASS；提交 `feat: release and reenroll cluster nodes safely`。

### Task 4: 网页配置保存、应用与手动证书

**Files:** Create `internal/daemon/local_config.go`, `internal/daemon/local_config_test.go`; modify `internal/daemon/daemon.go`, `internal/daemon/controller.go`, `internal/daemon/node.go`, `internal/control/local_http.go`。

**Interfaces:** 实现 Daemon.Status、SaveSettings、ApplySettings、UploadCertificate；内部 `applyLocalConfig(ctx context.Context, cfg config.Config) error` 只重启变化涉及的角色业务／derper，保持管理 Store、管理监听、账号和身份不变。

- [ ] 写 `TestLocalSettingsSaveApplyAndRolePersistence`：保存只改变 desired，Apply 后 current 与 desired 一致、PendingApply 为 false；初始选择两个角色并重启后均保持角色与管理员；已有主控／已配置成员修改角色被拒绝。
- [ ] 写 `TestApplyFailureKeepsManagementAndSavedState`：真实占用目标 TCP 端口，断言 Apply 返回错误、PendingApply 和 ApplyError 明确，管理登录仍成功；释放端口后重试成功。已有 child fake 用于验证取消控制流／停止／启动的先后顺序。
- [ ] 写 `TestManualCertificateUploadValidatesPairAndHostname`：有效完整链及匹配 key 可保存；不匹配 key／域名、超 1 MiB JSON 请求被拒绝且原运行证书未变；状态响应和日志不含 key。
  ```go
  if failedApply == nil || !status.PendingApply || status.ApplyError == "" || managementPage.Code != 200 { t.Fatal(status) }
  if retriedApply != nil || applied.PendingApply || applied.Active != applied.Saved { t.Fatal(applied) }
  ```
- [ ] 运行 `go test ./internal/daemon ./internal/control -run 'LocalSettings|ApplyFailure|ManualCertificate|LocalManagement' -count=1`，确认失败。
- [ ] SaveSettings 使用现有校验及 config.WriteAtomic；固定部署路径，公开端口保存在 Node.DERPPort／STUNPort。注册后的主控地址只能通过退出再加入改变；注册后的域名更改须与主控资源的域名变更及证明流程一致，不自行替换注册身份。
- [ ] ApplySettings 用 Daemon.opMu 串行执行：取消并等待旧节点控制流，停止需要替换的 derper，准备新策略客户端及角色业务，启动并验证子进程就绪，最后发布 current；失败保留可重试的 saved 状态，不报成功、不重建账号／私钥，不允许旧许可重新放行。
- [ ] 管理 Store 与监听不参与网页重启。主控只在角色选择后 EnableIdentity、ConfigureNodes 和启动同步；成员不启用这些业务。监测意外 child 退出时保留管理入口并报告中继不可用；修改对应现有生命周期测试的预期，不改变其他故障处理。
- [ ] 手动证书通过 tls.X509KeyPair 和 leaf.VerifyHostname 校验，在数据目录的独立新证书目录内写入完整 pair（0600），成功后原子保存 cert_dir；管理读取不返回 key，上传结果仍要求 Apply。保留既有手动和 letsencrypt 行为，不重写 TLS 实现。
- [ ] 重跑针对性测试与现有 `go test ./internal/daemon -run 'RestartOnly|NodeReload|ControllerLocalAdmin' -count=1`，预期 PASS；提交 `feat: save and apply local settings from web`。

### Task 5: 网页入群／退群与主控本机中继注册

**Files:** Create `internal/daemon/local_cluster.go`, `internal/daemon/local_cluster_test.go`; modify `internal/daemon/node.go`, `internal/control/local_http.go`, `internal/daemon/policy_runtime_test.go`。

**Interfaces:** 实现 Daemon.Join、Leave、RegisterLocal。Join 复用 EnrollmentClient.Enroll 与 pending receipt；RegisterLocal 复用 Store.CreateNode 和同一真实 HTTPS 注册入口，不直接写 registered 状态。

- [ ] 写 `TestWebJoinUsesProofAndRetriesPendingReceipt`：未入群实例保存主控 URL，域名证明必须能从管理入口访问，注册响应丢失后重试仍返回同一注册结果；不重复生成节点私钥。
- [ ] 写 `TestWebLeaveOnlineAndOfflineStopsRelayBeforeReturning`：两种连接状况下都先取消控制、停止 derper／既有连接，再持久化清空注册并删除缓存，重启不加载旧许可；管理员和 node.key 保留。在线返回 released=true，主控无法访问返回 released=false，同时本机已退出。
- [ ] 写 `TestWebRegisterLocalPreservesAuthorizationRules`：主控管理员注册本机中继成功，失败重试使用对应 pending 资源；普通用户被拒绝，重复成功注册不新建资源；尚未取得使用授权时无设备许可。
  ```go
  if exitErr != nil || after.Joined || after.Control.Usable || afterRestart.Joined { t.Fatal(after, afterRestart, exitErr) }
  if !bytes.Equal(keyBefore, keyAfter) || sessionRequest.Code != 200 { t.Fatal("exit lost local identity or login") }
  ```
- [ ] 运行 `go test ./internal/daemon -run 'WebJoin|WebLeave|WebRegisterLocal' -count=1`，确认失败。
- [ ] Join 仅对未注册成员接受 origin/code，在变更 URL 前停止旧控制流并处理旧 pending 请求；使用原私钥 responder；注册后按原流程启动控制流及策略应用，等待状态 ACK 单独显示。
- [ ] Leave 先取消控制流并停止中继，然后通过 ClearRegistration 保存本地不再注册的状态，再清理旧缓存并使用其返回的旧会话尝试主控释放（最多 5 秒）；在线释放失败不能阻止本机退出，但本地持久保存失败必须报告失败且保持中继停止。完成后可以启动没有任何许可的管理／中继端点。主控本机中继退出不关闭主控业务。
- [ ] RegisterLocal 使用当前 hostname 和公开端口创建管理员拥有的资源；已有相同 pending 本机资源复用并重新签发 code，已注册本机拒绝重复创建。不把注册码返回给普通配置响应或日志。
- [ ] 重跑针对性测试；真实 child 断流验证归入 Task 8。提交 `feat: manage local relay membership from web`。

### Task 6: 持久本地总限速与节点公开端口

**Files:** Modify `internal/control/node_self.go`, `internal/control/resources.go`, `internal/cluster/node_client.go`, `internal/cluster/control.go`, `internal/cluster/client.go`, `internal/derper/policy.go`, `internal/derper/policy_test.go`, `internal/derper/policy_integration_test.go`, `internal/daemon/local_cluster.go`, `internal/daemon/local_config.go`, `internal/control/local_http.go`, `patches/tailscale/uniderp.patch`, `patches/tailscale/uniderp.sha256`; create `internal/control/node_self_test.go`。

**Interfaces:** `Store.SetNodeSessionPorts(ctx context.Context, token string, derpPort, stunPort int) error` 与 `EnrollmentClient.SetPorts(ctx,derpPort,stunPort)` 使用 POST `/cluster/v1/node/ports`，节点 ID 只从 Token 推导。patched derpserver 的 `EnableUniDERP(maxBudgetBPS uint64)` 在子进程启动时接收本地限制；`UniDERPAppliedPolicy`、policyReply 和 `cluster.PolicyApplication` 增加 `EffectiveBudgetBPS uint64`，仅通过本地 IPC 返回实际调度预算。

- [ ] 将现有 host-budget 拒绝测试改为 `TestLocalBudgetClampsCachedAndHotPolicy`：主控预算 100000000／本地限制 80000000 时缓存及热应用均成功，RX／TX scheduler 的 budget.rate 为 10000000 bytes/s；热更新预算 200000000 后仍受相同本地限制，策略 revision 正常推进、原始缓存和摘要保持主控原值。主控预算 40000000 时有效预算 40000000；本地限制为 0 时遵守主控预算。直接调用 startUniDERPPolicy 传入 1–7 或超过 math.MaxInt64 的非零限制必须报错，避免 bytes/s 换算得到无限预算。
- [ ] 写 `TestLocalBudgetPreservesPolicyAndPriority`：分组／Tailnet 权重与上限、设备键、授权版本和三类许可截止时间与主控策略逐一相等；用既有 fake clock scheduler 测试验证压低总预算后仍遵守分组权重及单独上限，不将超限视为策略失败。
- [ ] 写 `TestLocalBudgetSurvivesUpdatesOfflineApplyAndRestart`：本地配置限制 80000000，连续接收不同主控预算不改写它；主控离线时在网页修改为 40000000，保存并应用后使用原缓存中仍有效的许可及新本地限制，重启继续为 40000000。策略已经到期时不恢复许可。
- [ ] 写 `TestNodeSelfPortsAreInstanceBound`：公开 DERP 3489／STUN 3488 写入自己的资源并反映到 DERP map；非法端口、额外 node_id、其他节点及过期／已释放 Token 拒绝。
  ```go
  if applied.Revision != incoming.Revision || !applied.Usable || applied.EffectiveBudgetBPS != 80000000 { t.Fatal(applied) }
  if !reflect.DeepEqual(incoming, cached) || !bytes.Equal(cacheBefore, cacheAfterLocalApply) { t.Fatal("local limit rewrote controller policy") }
  if rxBudget != 10000000 || txBudget != 10000000 || saved.Node.MaxBudgetBPS != 80000000 { t.Fatal("local cap not enforced or persisted") }
  ```
- [ ] 运行根模块针对性测试 `go test ./internal/derper ./internal/daemon ./internal/control -run 'LocalBudget|NodeSelfPorts' -count=1`，并在现有 builder 生成的 patched source 内运行 `go test ./derp/derpserver ./cmd/derper -run 'LocalBudget' -count=1`，确认失败。
- [ ] PolicyClient 完整保存主控策略并通知 child，不对超出本地限制的预算返回错误。derper 的 cache／digest／binding 验证维持原样；仅在 ApplyUniDERPPolicy 建立 RX／TX 调度器时计算有效预算：本地限制为 0 时用主控预算，否则取 min；保留原 policy/digest。EnableUniDERP 把限制保存在 Server.uniderpLocalBudgetBPS，不随策略替换；startUniDERPPolicy 校验范围并通过既有启动参数传入，修改后由 Task 4 的保存并应用重启 derper、加载原缓存；持续管理入口不受影响。
- [ ] 实际 IPC 状态返回调度预算，换算 bps 时遵循现有整数 bytes/s 调度精度；状态页面把已接收的主控预算与当前调度预算分开。主控策略更新不写本机 YAML，退出及重新加入也保留本地 MaxBudgetBPS。
- [ ] 清除已无调用方的 ErrHostBudget 和 host_budget 专用分支，将原测试改为正常应用／缓存／取消行为验证；不留下记录旧拒绝行为的兼容逻辑。节点自管理只增加已有公开端口更新所需的权限，复用 SetNodePorts 事务校验。
- [ ] 更新 patch hunk 行数和 SHA256，使用现有 build-derper 的校验及 upstream tests 验证可应用。重跑新测试和主控既有 `go test ./internal/control -run 'QoSOnlyProvider|NodePublicPorts' -count=1`，预期 PASS；提交 `feat: clamp relay budget to persistent local limit`。

### Task 7: 首次角色向导、成员界面与本机设置页

**Files:** Modify `internal/control/ui/app.js`, `internal/control/ui/index.html`, `internal/control/ui/app.css`, `internal/control/ui_test.go`。

**Interfaces:** session 的 server_role 决定向导／主控／成员导航；页面使用 Task 2 的 DTO 与路径，主控原 views 不变。新增 `bootstrap()`、`localSettings()`、`member()` 页面函数；本地总限速属于本机设置。

- [ ] 扩展现有 UI 路由测试：初始／成员／主控共享登录资产；每种状态下对应后台接口都通过真实 handler 验证，避免只测试字符串存在。先运行针对性测试确认尚未满足。
  ```go
  if page.Code != 200 || anonymousLocalWrite.Code != 401 { t.Fatal("shell must not grant authority") }
  if memberTailnets.Code != 404 || memberLocalStatus.Code != 200 || mainOrdinaryLocalWrite.Code != 403 { t.Fatal("role route boundary") }
  ```
- [ ] 登录后 setup 显示“配置为主控”“加入已有集群”；角色表单保存并应用域名／监听／TLS；成员未入群可先保存配置，再填写主控地址及注册码。敏感输入使用 new-password，提交后清空，不写 location/hash。
- [ ] 主控管理员添加“本机设置”入口及“注册本机中继”；成员账号在主控按现有资源所有权管理主要限速与权重。成员节点仅显示本机状态、集群连接、本机设置、我的账号；退出前明确确认，成功后显示未加入状态。
- [ ] 本机设置提供“本地总限速（Mbps，0 表示不额外限制）”，转换为 bps；状态显示已接收的主控预算、本地限制、derper 当前有效预算和本机接收／应用版本。分组和优先级规则按收到的策略只读展示。主控失联时仍可保存并应用本地限制，明确应用时会重启中继；页面不把“保存”等同于“应用”。
- [ ] 设置页显示 saved／active 差异及 ApplyError，证书上传与“保存并应用”有明确结果；发生中继连接重启时通过持续可用的管理入口核对状态。主控失联仍可登录、修改本机配置、退出和改密码。
- [ ] 运行 `node --check internal/control/ui/app.js` 及相关 handler 测试；用临时实例在真实浏览器走两个角色分支、离线成员修改本地限制、超限策略成功应用、证书／监听校验，检查窄窗口布局。提交 `feat: add bootstrap and member management pages`。

### Task 8: 部署接线、针对性验收与测试服务器更新

**Files:** Modify `Dockerfile`, `docker-compose.example.yaml`, `docker-compose.node.example.yaml`, `config.example.yaml`, `config.node.example.yaml`, `README.zh-CN.md`, `README.md`; extend `internal/daemon/policy_runtime_test.go`；刷新忽略目录中的手动部署 Compose／README，保留 OAuth 凭据。

**Interfaces:** 独立管理 HTTP 容器端口 3378；HTTPS 443 下 `/manage/`、`/api/v1/`、`/cluster/v1/` 指向管理入口，`/derp`、`/derp/*` 及原探测数据路径仍指向 derper。管理后端只发布到宿主机 loopback。

- [ ] 增加真实 child 测试 `TestLocalConsoleLeaveClosesExistingDERPConnections`：从实际 patched derper 建立授权 DERP 连接，网页退出后连接关闭，再重启仍拒绝旧设备；管理页面始终可请求。复用现有 policy_runtime_test 的测试二进制开关及已有客户端测试方式。
  ```go
  if receiveAfterLeave == nil || connectOldDeviceAfterRestart == nil || managementPage.Code != 200 { t.Fatal("leave did not close relay while preserving console") }
  ```
- [ ] 运行 `go test ./internal/config ./internal/control ./internal/cluster ./internal/daemon ./internal/derper ./cmd/uniderp -count=1`、对应 `go vet`、JS 语法检查和 `git diff --check`；仅对新生命周期并发测试运行受支持平台上的 `-race`。
- [ ] 更新 Compose 的 loopback 管理端口发布和示例配置。README 补充首次网页设置、主控／成员流程、独立管理入口的 HTTPS 反代路径、公开端口与实际监听区别、限速保存／应用语义；TLS 仍按现有 external／passthrough 方式部署。无反代的 passthrough 部署须另外准备管理入口的 HTTPS，确保中继停止时仍可管理。
- [ ] 用标准 Dockerfile 缓存构建最新产品 commit，验证实际二进制版本和镜像中页面；不强制无缓存重建。构建包含当前 patched upstream 的原有测试。
- [ ] 更新前只读记录 `/opt/uniderp-manual` 的数据库／账号／资源与持久密钥摘要，保存现有 Compose／配置和 nginx 配置供恢复；不打印或重建 OAuth，不删除现有数据。主控管理后端发布 `127.0.0.1:3387:3378`，原 DERP 后端 `127.0.0.1:3388:3377` 保留；nginx 先配置管理路径并做语法检查，再 reload。
- [ ] 主控原地更换镜像，验证原账号／Tailnet／资源和密钥保持；新增成员使用独立容器与数据目录、已放行的 TCP／UDP 3489 和独立 HTTPS 域名／反代。通过网页设置管理员、入群、设置本地限速，再由主控成员账号保存超过本地限制的预算和权重；核对成功 ACK、运行调度预算及实际 DERP 转发仍可用。验证离线修改本地限速、重启保持、在线／离线退出及重新注册后的限制保持与真实连接关闭。保留生产 multiderp 域名，不另做 STUN 扩展验收。
- [ ] 检查最终 diff，去掉非任务所需的修改；提交部署／验收涉及的产品文件。输出最新本地镜像标签、可用管理 URL、当前验收结果及必要的部署差异；手动部署数据继续保留供用户复核。

## Execution handoff

建议 Native：上述任务共享 Daemon 生命周期、认证与注册状态，在当前会话顺序实施可减少接口交接成本。产品实现前由用户审阅本计划并选择执行方式。
