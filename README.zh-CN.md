[English](README.md) | 简体中文

# MultiDERP

MultiDERP 让一台自建的 Tailscale DERP 服务器同时服务多个彼此独立的 tailnet。每个 tailnet 对应一个独立的 `tsnet` 验证器身份；有客户端连接 DERP 时，这些验证器负责确认节点密钥属于哪个已配置的 tailnet，再决定是否放行。

实际转发流量的是上游 Tailscale `derper`。镜像中的 `derper` 与验证器代码来自同一个固定版本的 Tailscale Go module。多个 tailnet 共享的是 DERP 入口和准入流程，各自的身份、访问策略和控制面成员关系仍然独立。

## 工作方式

```text
                         Tailscale 控制面
                         ▲         ▲         ▲
                         │         │         │
                  ┌──────┴──┐ ┌────┴────┐ ┌───┴──────┐
                  │ 验证器 A │ │ 验证器 B │ │ 验证器 C │
                  │tailnet A│ │tailnet B│ │tailnet C │
                  └──────┬──┘ └────┬────┘ └───┬──────┘
                         │          │           │
                         └──────────┼───────────┘
                                    │ 节点密钥成员查询
                             ┌──────▼──────┐
DERP 客户端 ────────────────►│   准入层    │
                             └──────┬──────┘
                                    │ allow / deny
                             ┌──────▼──────┐
                             │   derper    │
                             │ TLS + STUN  │
                             └─────────────┘
```

验证器连上 Tailscale 并完成 hardening 配置与回读校验后，才会进入准入池。每次 DERP 准入请求都会针对当前可用的验证器集合检查节点密钥；任意一个有效验证器确认成员关系即可通过。

多个 tailnet 因而可以共用一台 relay host，同时保持各自的网络边界。节点身份、ACL/Grants、WireGuard 密钥和端到端加密仍由各自的 Tailscale tailnet 管理。

## MultiDERP 负责什么

- 为每个 tailnet 保存独立的 Tailscale 验证器状态；
- 通过网页登录、OAuth 或 auth key 完成验证器入网；
- 根据节点密钥成员关系处理 DERP 准入；
- 管理一个上游 `derper` 子进程；
- 支持外部 TLS 终止，也支持由 `derper` 直接处理 TLS；
- 单独提供 UDP STUN 监听；
- 通过 Unix socket 提供本地管理接口；
- 持久化验证器状态和移除后的 orphan state；
- 提供 liveness、readiness 和 startup 健康检查。

V1 面向 Tailscale 官方控制面，配置范围集中在多 tailnet 私有 DERP 准入。DERP mesh 以及上游实验性的速率/连接数控制目前没有对应的 V1 配置入口。

## 部署前准备

常规容器部署需要：

- Docker Engine（仓库提供的部署示例使用 Docker Compose）；
- 一个给 DERP 使用的公网 DNS 名称；
- 可持久写入的 `/data`；
- 公网 DERP HTTPS 入口的 TCP 连通性；
- 如果使用内置 STUN，则需要 UDP `3478`；
- 各个验证器到 Tailscale 控制面的出站网络；
- 每个准备使用该 DERP 的 tailnet 的管理权限。

仓库提供的 Compose 示例会让容器以 UID/GID `10001:10001` 运行，并使用只读根文件系统，因此宿主机挂载到 `/data` 的目录需要允许这个身份写入。

从源码构建时，仓库当前声明的 Go 版本为 `1.26.6`。

## 部署方式一：外部 TLS

这是示例配置的默认方式。公网 HTTPS 由前置 TLS 终止器处理，然后把 DERP 后端流量转给 MultiDERP 的私有监听端口。

```text
Internet
   │
   │ TCP 443
   ▼
TLS 终止器 / 兼容的反向代理
   │
   │ 明文 DERP 后端流
   │ 127.0.0.1:3377
   ▼
MultiDERP / derper

Internet ───────── UDP 3478 ─────────► STUN
```

仓库自带的 Compose 示例把 TCP `3377` 绑定在宿主机 loopback，只把 UDP `3478` 单独发布出去。

### 1. 准备数据目录

```bash
git clone https://github.com/lsy223622/MultiDERP.git
cd MultiDERP

mkdir -p data
cp config.example.yaml data/config.yaml
```

Linux 普通 bind mount 可以直接把目录交给容器使用的 UID/GID：

```bash
sudo chown -R 10001:10001 data
```

Docker Desktop 等环境的所有权处理可能不同；核心要求是容器内的 `10001:10001` 能在 `/data` 下创建文件并完成原子替换。

### 2. 设置公网主机名

编辑 `data/config.yaml`：

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

### 3. 启动

```bash
docker compose -f docker-compose.example.yaml up -d
```

示例会发布：

```text
127.0.0.1:3377 -> container :3377/tcp
0.0.0.0:3478   -> container :3478/udp
[::]:3478      -> container :3478/udp
```

外部 TLS 模式下，`3377` 承载的是明文 DERP 后端流，所以示例把它限制在宿主机 loopback。公网入口应该落在前面的 TLS 终止器上。

DERP 会保持长连接，并在升级后切换到自己的协议。选用反向代理时需要确认它能完整承载这条连接；普通 HTTP 代理的默认配置并不等价于 DERP 兼容的代理路径。

### 4. 配置公网 TLS

让 TLS 终止器为 `server.hostname` 提供 HTTPS，并把 DERP 后端连接转到：

```text
http://127.0.0.1:3377
```

STUN 是独立的 UDP `3478` 流量，直接经过宿主机和网络防火墙，不走 HTTP 反向代理。

## 部署方式二：Let's Encrypt 直连 TLS

`tls_mode: passthrough` 会把公开 TLS 交给 `derper` 自己处理。

对应配置：

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

使用直连 TLS 的 Compose 示例：

```bash
docker compose -f docker-compose.letsencrypt.example.yaml up -d
```

这个示例发布 TCP `80`、`443` 和 UDP `3478`。容器继续以非 root 身份运行，Compose 文件只额外增加 `NET_BIND_SERVICE`；绑定低端口不需要 `NET_ADMIN` 或 privileged 模式。

配置校验器会检查 TLS 组合：

| TLS 模式 | 证书模式 | 监听规则 |
| --- | --- | --- |
| `external` | `none` | DERP 后端使用非 443 的内部端口，`cert_dir` 为空。 |
| `passthrough` | `manual` | 需要 `cert_dir`，TLS 监听可使用自定义端口。 |
| `passthrough` | `letsencrypt` | 需要 `cert_dir`，DERP 监听 TCP `443`。 |

## 添加 tailnet

`tailnets` 可以从空列表启动。守护进程起来以后，通过管理 CLI 添加验证器身份。

使用仓库的 Compose 示例时，可以直接在容器里执行：

```bash
docker exec multiderp multiderp tailnet list
```

### 网页登录

```bash
docker exec multiderp multiderp tailnet add personal
```

网页登录是默认 enrollment 方式。命令会返回一个 Tailscale 登录 URL；用有权限把节点加入目标 tailnet 的账号完成登录，然后查看状态：

```bash
docker exec multiderp multiderp tailnet status personal --verbose
```

### OAuth

把 OAuth client secret 放进受保护的文件，再传入文件路径和至少一个 tag：

```bash
docker exec multiderp multiderp tailnet add work \
  --oauth-secret-file /data/secrets/work-oauth \
  --tag tag:multiderp
```

OAuth enrollment 使用 secret 文件和 tags。敏感值本身留在 YAML 之外。

### Auth key

```bash
docker exec multiderp multiderp tailnet add lab \
  --auth-key-file /data/secrets/lab-auth-key
```

这个文件属于部署敏感状态，宿主机权限应按凭据文件处理。

### Required 验证器

当某个验证器的可用性需要参与整个服务的 readiness 时，可以加 `--required`：

```bash
docker exec multiderp multiderp tailnet add work \
  --oauth-secret-file /data/secrets/work-oauth \
  --tag tag:multiderp \
  --required
```

验证器处于 disabled 状态时，effective required 会暂时变为 false。`tailnet status --verbose` 会同时显示配置值和实际生效值。

## 在每个 tailnet 中发布 DERP

每个 tailnet 都通过自己的 Tailscale policy 管理 DERP map。需要使用这台服务器的 tailnet，应分别添加 custom DERP region，并填写部署时实际使用的公网主机名和端口。

Tailscale 当前的 custom DERP 文档和 policy 语法：

<https://tailscale.com/docs/reference/derp-servers>

修改 policy 后，可以使用 `tailscale netcheck` 检查客户端收到的 DERP 区域及连通情况：

```bash
tailscale netcheck
```

DERP map 负责让客户端发现服务器；MultiDERP 的准入层再根据节点密钥判断这个连接属于哪个已配置的 tailnet。

## 配置文件

配置格式为 YAML，schema 版本是 `1`。容器 entrypoint 默认读取：

```text
/data/config.yaml
```

### Server

| 字段 | 默认值 | 作用 |
| --- | --- | --- |
| `server.hostname` | 空 | 公网 DERP 主机名；启用验证器后需要有效值。 |
| `server.derp.listen` | `:3377` | DERP TCP 监听。 |
| `server.derp.stun_listen` | `:3478` | STUN UDP 监听。 |
| `server.derp.tls_mode` | `external` | `external` 或 `passthrough`。 |
| `server.derp.cert_mode` | `none` | `none`、`manual`、`letsencrypt`，受 TLS mode 约束。 |
| `server.derp.cert_dir` | 空 | passthrough TLS 使用的证书目录。 |
| `server.admin.socket` | `/run/multiderp/admin.sock` | 本地管理 Unix socket。 |
| `server.health.listen` | `127.0.0.1:9090` | 健康检查 HTTP 监听。 |

如果 DERP 和 STUN 的监听地址都显式写了 host，配置校验要求二者使用同一个 host。

### Storage

| 字段 | 默认值 | 作用 |
| --- | --- | --- |
| `storage.state_dir` | `/data` | 应用顶层状态目录。 |
| `storage.tailnet_state_dir` | `/data/tailnets` | 各验证器的 Tailscale 状态。 |
| `storage.orphan_state_dir` | `/data/orphans` | 验证器移除后保留的状态。 |

### Logging

`logging.level` 可选：

```text
debug
info
warn
error
```

默认是 `info`。

### Tailnet 条目

规范化后的验证器条目大致如下：

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

`name` 是 MultiDERP 内部使用的验证器标识，最大 64 字符，可使用字母、数字、`-`、`_` 和 `.`。省略 `hostname` 时会生成 `multiderp-<name>`。

不同 `auth.type` 对应的材料：

| 类型 | 需要的材料 |
| --- | --- |
| `web` | 交互式登录；不得配置 secret 文件。 |
| `oauth` | `client_secret_file` 和至少一个 tag。 |
| `auth_key` | `auth_key_file`。 |

验证器配置和状态有生命周期关系，因此日常增删改更适合通过 CLI 完成。

## 管理 CLI

当前命令结构：

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

`serve` 默认使用 `127.0.0.1:3340` 作为本地 admission callback 地址。只有在部署拓扑确实需要其他地址时，才使用 `--admission-address` 覆盖默认值。

容器里最常用的是：

```bash
docker exec multiderp multiderp tailnet list
docker exec multiderp multiderp tailnet status personal --verbose
docker exec multiderp multiderp config reload
```

### 启用和停用

```bash
docker exec multiderp multiderp tailnet disable personal
docker exec multiderp multiderp tailnet enable personal
```

disable 会把验证器移出准入池，同时保留配置和状态，后续可以再次 enable。

### 移除、orphan 和永久清理

```bash
docker exec multiderp multiderp tailnet remove personal
docker exec multiderp multiderp orphan list
```

移除验证器时，原状态会完整转移到 orphan state。永久删除是单独的操作：

```bash
docker exec -it multiderp multiderp orphan purge <orphan-id>
```

`orphan purge` 会删除保留的验证器状态。自动化脚本只有在已经明确做出这个不可逆决定时才适合加 `--yes`。

### 配置热重载

```bash
docker exec multiderp multiderp config reload
```

普通配置可以通过 reload reconcile；已有验证器的认证类型、secret 文件路径、tags 和验证器 hostname 属于身份敏感字段。需要调整这些身份关系时，使用对应的生命周期命令更合适。

同理，移除验证器走 `tailnet remove`，让守护进程有机会把旧状态完整放入 orphan 区域。

## 验证器状态与准入

验证器可能处于：

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

进入准入池的条件比“进程已经启动”更严格：

```text
state == connected
and hardening_verified == true
```

verbose 状态还可以看到：

- 认证方式；
- configured/effective required；
- 当前是否参与 admission；
- 可用时的登录 URL；
- tailnet 和节点身份；
- node key 与 Tailscale IP；
- state directory；
- 最近一次错误。

### Hardening 基线

验证器参与准入前，MultiDERP 会应用并回读一套最小能力配置。当前固定版本检查的内容包括：

- Shields Up 开启；
- remote configuration 关闭；
- route-all 关闭；
- exit node 为空；
- advertised routes/services 为空；
- Tailscale SSH 和 Web client 关闭；
- Serve/Funnel/services 为空；
- App Connector 关闭；
- posture checking 关闭；
- auto update 关闭；
- drive shares 为空；
- relay-server 设置为空；
- backend 正在运行并具有 node key。

如果回读发现 drift，验证器会先退出准入池，再进入修复流程。若固定版本的 LocalAPI 行为与预期矩阵不兼容，验证器会进入 error 状态，把依赖/API 不匹配与普通短暂网络抖动明确区分开。

完整矩阵在 [`HARDENING-COMPATIBILITY.md`](HARDENING-COMPATIBILITY.md)。

### Admission 并发和超时

当前控制器使用有限队列和超时：

| 限制 | 当前值 |
| --- | ---: |
| 单次 admission 请求超时 | 4 s |
| 单个验证器查询超时 | 2 s |
| 同时处理的 admission 请求 | 64 |
| 同时进行的 verifier 查询 | 32 |
| verifier job 队列 | 256 |

这些值目前属于 V1 实现限制。准入会基于当前 verifier pool 的快照查询，并在最终放行前确认给出允许结果的验证器仍然是当前实例。

## 健康检查

健康监听提供：

```text
/health/live
/health/ready
/health/startup
```

对应条件满足时返回 HTTP `200`，否则返回 `503`。

健康快照会反映：

- 进程 liveness；
- startup 是否完成；
- `derper` 是否可用；
- 可参与 admission 的验证器数量；
- required verifier 失败；
- 是否存在 pending restart。

默认地址为 `127.0.0.1:9090`。容器里的 loopback 属于容器自己的 network namespace；外部编排器需要直接探测时，再按实际监控拓扑调整监听和端口发布。

## 持久化状态

`/data` 可能包含不同类型的运行状态：

```text
/data/config.yaml
/data/tailnets/...
/data/orphans/...
/data/certs/...        # 直连 TLS
/data/secrets/...      # 如果凭据文件采用这个目录布局
```

验证器目录中包含 Tailscale 节点身份和 enrollment 状态，因此 `/data` 的备份应按生产主机敏感状态保护。

验证器生命周期命令会维护配置与状态之间的对应关系：`tailnet remove` 把旧状态移到 orphan 区，`orphan purge` 才执行最终的不可逆清理。

## 安全模型

MultiDERP 的 operator 掌握宿主机、验证器状态、配置和准入服务，因此 root 或等价的宿主机管理员位于信任边界之内。

DERP 中继时，Tailscale 的 WireGuard 端到端加密仍然覆盖 peer payload。DERP 主机负责转发可用性并持有用于成员查询的验证器身份，而 peer 之间的数据内容继续由 Tailscale 加密。

各 tailnet 的验证器被设置成低能力节点，完成 hardening 回读以后才参与 admission，运行期间也会继续检查 drift。需要更强控制面隔离时，tailnet owner 还可以针对专用 verifier tag 配置自己的 Tailscale Grants/ACL。

本地管理面和持久化状态应该落在同一个 operator 信任边界里：

- `/run/multiderp/admin.sock` 具有管理权限；
- verifier state 保存 Tailscale 节点身份；
- OAuth/auth-key 文件属于 enrollment 凭据；
- external TLS 的后端监听传输明文 DERP backend stream。

这些接口和文件适合放在受控的本地或私有网络路径上。

UDP `3478` 的 STUN 用于 endpoint discovery；真正的 DERP 成员准入仍由 verifier callback 路径决定。

漏洞报告流程见 [`SECURITY.md`](SECURITY.md)。

## 运维说明

### DERP 通常处在回退路径

Tailscale 会优先尝试直连；当前版本也可以在配置 Peer Relay 后使用 Peer Relay，DERP 则承担更通用的回退中继。自建 DERP 的主要价值通常是掌控中继位置，或者给参与的 tailnet 提供更合适的 fallback 网络位置。

Tailscale 当前文档也列出了 custom DERP 与跨 tailnet sharing 等能力之间的边界，部署时建议以最新上游行为为准：

<https://tailscale.com/docs/reference/derp-servers>

### STUN 和 DERP 是两条网络路径

DERP 使用 TCP/TLS，STUN 使用 UDP `3478`。UDP 连通性需要独立检查；STUN 直接经过宿主机和网络防火墙。

### hostname 变更需要联动

`server.hostname` 是各 tailnet DERP map 中看到的公网名称。迁移 hostname 时，通常要一起处理 DNS、证书、MultiDERP 配置和各 tailnet policy。

## 排障

### 服务已经运行，但客户端仍被拒绝

先检查 verifier pool：

```bash
docker exec multiderp multiderp tailnet list
docker exec multiderp multiderp tailnet status <name> --verbose
```

可用于 admission 的验证器应处于 `connected`，并且 hardening verified。`waiting-login`、`degraded`、`error`、`disabled` 等状态通常能直接解释“derper 在运行但成员准入失败”的情况。

### 网页 enrollment 一直停在登录阶段

```bash
docker exec multiderp multiderp tailnet login <name>
```

用目标 tailnet 的账号完成返回的 Tailscale 登录 URL，再查看 verbose 状态。

### reload 拒绝身份相关配置

认证方式、secret 路径、tag、hostname 和 verifier removal 适合走生命周期命令。这样现有 state directory 会继续对应创建它的那个验证器身份。

### HTTPS 正常，STUN 不通

单独检查 UDP `3478`：宿主机防火墙、云安全组/防火墙和前置 NAT 都可能影响它。

### 客户端看不到 custom DERP region

DERP region 来自各 tailnet 的 Tailscale policy。先检查 policy，再查看客户端视角：

```bash
tailscale netcheck
```

### 容器无法写入 `/data`

镜像以 UID/GID `10001:10001` 运行。检查 Docker host 上 bind mount 的 owner 和权限即可。

### 宿主机访问不到 health port

默认 `127.0.0.1:9090` 在容器内部，并且示例 Compose 没有发布这个端口。只有实际监控拓扑需要外部访问时才需要额外暴露或改监听地址。

## 构建与测试

Go module：

```text
github.com/lsy223622/MultiDERP
```

构建 MultiDERP 和固定版本的上游 DERP：

```bash
go build ./cmd/multiderp
go build tailscale.com/cmd/derper
```

常规检查：

```bash
go test ./...
go vet ./...
```

涉及并发的修改还可以跑：

```bash
go test -race ./...
```

CI 还会覆盖对应的构建目标和容器镜像。

## Tailscale 依赖版本

仓库当前固定：

```text
tailscale.com v1.102.3
```

Dockerfile 中的 `derper` 也从同一个 module 版本构建。升级 Tailscale 会同时改变 relay binary 和 hardening/admission 依赖的 verifier API，因此应审阅 [`HARDENING-COMPATIBILITY.md`](HARDENING-COMPATIBILITY.md)，并重新运行仓库中的相关单元测试、竞态测试、构建检查和容器构建。

## 安全问题报告

安全问题使用 GitHub 私有漏洞报告：

<https://github.com/lsy223622/MultiDERP/security/advisories/new>

凭据、private node key、verifier state、证书私钥等敏感内容应只放进私有报告。

## 许可证

MultiDERP 使用 [GNU General Public License v3.0](LICENSE)。

构建产物还包含上游 Tailscale 代码；重新分发 binary 或 image 时，请同时检查仓库中的第三方许可证材料。
