[English](README.md) · [简体中文](README.zh-CN.md)

# MultiDERP

MultiDERP 是围绕上游 Tailscale derper 构建的一层小型准入控制层。一个守护进程可以为多个 Tailnet 运行相互独立的 tsnet.Server 验证器，同时 DERP 数据通路仍由上游子进程负责。只有当至少一个符合条件的验证器使用 WhoIsNodeKey 确认客户端的 NodePublic 后，客户端才会获准接入。

V1 的边界经过刻意收窄：

- 仅支持 Tailscale 官方控制平面；拒绝 `control_url`。
- 每个验证器使用一个相互隔离的状态目录和 tsnet 身份。
- 启用 ShieldsUp，不提供 routes/services/SSH/Web/Serve/Funnel，禁用 Remote Config，并在准入前执行加固配置回读检查。
- 不支持 DERP mesh、本地 tailscaled 客户端验证、配额或自定义 DERP 数据通路。
- 配置面为 YAML 和 secret 文件。secret 值永远不会写入 YAML，也不会通过管理 API 返回。

## 快速开始

示例配置只包含占位符。请替换公共主机名，并通过部署使用的受保护 secret 机制提供 secret 文件。

Compose 示例使用公开发布的镜像 `ghcr.io/lsy223622/multiderp:latest`。发布标签还会发布类似 `ghcr.io/lsy223622/multiderp:1.0.1` 的版本标签；当部署不应自动跟随后续版本时，请使用该标签或镜像 digest。
`latest` 标签只会由稳定的 `vX.Y.Z` 版本更新；运行中的容器仍需要镜像更新器，或者需要定期执行 `docker compose pull` 和 `docker compose up -d`，才能应用该更新。

Linux/macOS：

~~~sh
mkdir -p data
cp config.example.yaml data/config.yaml
docker compose -f docker-compose.example.yaml up -d
~~~

PowerShell：

~~~powershell
New-Item -ItemType Directory -Force data
Copy-Item config.example.yaml data\config.yaml
docker compose -f docker-compose.example.yaml up -d
~~~

如果不存在 `data/config.yaml`，容器会根据内置的 `config.example.yaml` 自动创建它。上面的显式复制步骤适用于希望在首次启动前替换示例主机名的情况。

示例会有意以 `tailnets: []` 启动。在此状态下，管理器、管理 socket 和健康检查服务器可以运行，但不会启动 derper 子进程，并且 readiness 会保持为 false。请通过 Unix 管理 socket 添加第一个验证器：

~~~text
docker exec multiderp multiderp tailnet add alice
docker exec multiderp multiderp tailnet list
docker exec multiderp multiderp tailnet status alice
~~~

如需非交互式注册，请指向一个只读 secret 文件：

~~~text
docker exec multiderp multiderp tailnet add company --oauth-secret-file /run/secrets/company-oauth --tag tag:multiderp-verifier
docker exec multiderp multiderp tailnet add lab --auth-key-file /run/secrets/lab-auth-key
~~~

OAuth 注册至少需要一个要发布的标签。使用多个 `--tag` 可重复指定多个标签；其顺序会在管理请求和验证器配置中保留。对应的 YAML 条目如下：

~~~yaml
auth:
  type: oauth
  client_secret_file: /run/secrets/company-oauth
  tags:
    - tag:multiderp-verifier
~~~

Web 身份验证会返回一个 Tailscale 登录 URL。该 URL 只表示需要登录，并不表示验证器已经具备准入资格。登录后，MultiDERP 会再次应用并回读加固基线，然后才把验证器发布到准入池中。

## 配置与生命周期

如果 `/data/config.yaml` 不存在，系统会根据内置的 `config.example.yaml` 自动创建它。生成的文件包含示例中的默认服务器、存储和日志值，并将 `tailnets` 设置为 `[]`；它不会重新创建之前运行时中可能存在的验证器条目。示例主机名仍是占位符，应在添加启用的验证器之前替换它。

生成的配置会启动管理器、管理 socket 和健康检查服务器，但不会启动 derper 子进程，因此在添加验证器且验证器具备准入资格之前，readiness 会保持为 false。现有的空文件、null 文件、仅包含注释的 YAML 文件或 `{}` 文件，都表示相同的空目标配置。无效或无法读取的文件仍会导致启动失败。

守护进程是权威写入者。运行时变更通过管理 socket 进行，会经过验证，使用临时文件加 fsync 和原子重命名写入，然后才进行协调。也可以通过手动编辑配置后执行以下命令重新加载：

~~~text
docker exec multiderp multiderp config reload
~~~

无效的 reload 会保持当前运行时不变。监听器、TLS/证书、管理、健康检查、存储根目录和服务器主机名的变更会持久化为待重启变更；在守护进程重启前，当前监听器、子进程和状态根目录会继续使用。
在复用现有状态时，验证器身份、认证或主机名变更会被拒绝；请改为执行显式 reset 或 remove-and-add 操作。
手动从 YAML 中删除验证器也会在 reload 时被拒绝；请使用 `tailnet remove <name>`，这样其状态会被移动到自动生成的 orphan 目录中。

常用操作：

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

`disable`、`logout`、`reset` 和 `remove` 会在关闭验证器前将其移出准入池。`reset` 要求 LocalAPI logout 成功后才会删除本地状态。`remove` 会将状态保留在自动生成的 orphan ID 下；只有显式确认的 orphan purge 才会删除它。同名的 add 操作永远不会自动加载 orphan。

`derp restart` 是全局会话撤销操作。它会暂时拒绝新的准入，停止子进程，然后使用相同的持久化 DERP key 和当前符合条件的验证器池重新启动子进程。意外的子进程退出会使父进程以非零状态退出，以便容器监督器重启完整服务。

`tailnet status --verbose` 会包含完整的验证器 NodeKey，供运维诊断使用；普通状态输出会对其进行脱敏。

## TLS、公共端点与后端监听器

MultiDERP 将公共 DERP 端点与本地 derper 进程使用的监听器分开。每个 Tailnet 的 DERPMap 中的 `HostName` 和 `DERPPort` 是客户端使用的公共端点；`server.derp.listen` 只是本地反向代理到达的监听器，或者在 derper 自行终止 TLS 时客户端到达的监听器。不要误把内部后端端口复制到公共 DERPMap 中。

例如，下面的 Nginx 部署具有这样的拓扑结构：

~~~text
DERPMap -> https://derp.example.com:443
Nginx   -> http://127.0.0.1:3377
~~~

默认后端端口是 TCP `3377`，属于部署内部端口；不得将其开放到公共 Internet。STUN 使用 UDP `3478`；Compose 示例会明确通过 IPv4 和 IPv6 直接发布该端口，或通过支持 UDP 的代理发布。主机和 Docker 守护进程必须启用 IPv6，IPv6 映射才能工作。公共 DERP 端点仍可以使用 TCP `443`；它与后端端口相互独立。

V1 只支持以下证书模式：

- `cert_mode: none` 配合 `tls_mode: external`：derper 提供 HTTP/DERP 后端，兼容的反向代理负责终止公共 TLS。
- `cert_mode: manual` 配合 `tls_mode: passthrough`：derper 使用提供的证书，并可以监听任意配置的 TCP 端口。
- `cert_mode: letsencrypt` 配合 `tls_mode: passthrough`：derper 获取并续期自己的证书，并且必须监听 443 端口。

V1 不支持 `cert_mode: gcp`，并会将其作为不支持的配置值拒绝。证书设置不会在外部终止和透传监听器模型之间混用。

### A. MultiDERP 在 443 上终止 Let's Encrypt

当 derper 应直接提供 HTTPS 时使用此模式。持久化证书目录，并在 Tailnet 的 DERPMap 中发布生成的公共端点：

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

DERPMap 条目为 `HostName: derp.example.com`、`DERPPort: 443` 和 `STUNPort: 3478`。在此模型中不存在单独的公共后端监听器。上游 HTTP-01 challenge 监听器会在 80 端口启用，因此签发和续期期间，公共 TCP 80 必须能够到达同一个容器。示例容器以 UID/GID 10001 运行；请使用 `docker-compose.letsencrypt.example.yaml` profile，该 profile 只授予 `NET_BIND_SERVICE`，使进程可以绑定 80 和 443。它不使用 `CAP_NET_ADMIN` 或 privileged 模式。请持久化 `/data`，使 ACME 状态和 DERP key 在重启后仍然保留。

### B. Nginx 在 443 上终止公共 TLS

当 Nginx 管理公共证书，而 MultiDERP 作为内部 HTTP/DERP 后端运行时，使用此模式：

~~~yaml
server:
  hostname: derp.example.com
  derp:
    listen: ":3377"
    stun_listen: ":3478"
    tls_mode: external
    cert_mode: none
~~~

DERPMap 仍然只包含 `derp.example.com:443`；客户端不知道 Nginx 会将请求转发到内部后端。默认 Compose 示例将该后端绑定到 `127.0.0.1:3377`，供主机上的 Nginx 使用。如果 Nginx 作为同一私有网络中的容器运行，请改用 `multiderp:3377`。请将 3377 保留在私有容器网络中，或只绑定到受保护的本地接口。不要将其作为第二个公共 DERP 端点发布。

如果部署明确需要另一个后端端口，`:8443` 是有效的自定义选择，但它不是默认值，所有代理 upstream 和端口映射都必须保持一致地修改：

~~~yaml
server:
  derp:
    listen: ":8443" # explicit custom backend port
~~~

DERP 是一个长期存在的双向升级流。Nginx 必须支持 HTTP/1.1，保留 Upgrade 和 Connection，为 /derp 禁用缓冲，并使用较长的读写超时。浏览器响应或普通 HTTP 200 不是 DERP 中继测试。

公共 HTTP 表面应采用 allowlist。请根据你的部署调整下面的公共证书配置：

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

不要发布 `/debug`、`/metrics`、管理 socket 或任意上游路径。如果所选代理无法承载 DERP 升级流，请使用 `tls_mode: passthrough` 配合 L4 TCP 转发器，并根据上面的证书模型选择 `manual` 或 `letsencrypt`。DERP map 在每个 Tailnet 的控制平面中配置；MultiDERP 不会创建、重写或删除 DERP map 区域。

## 健康检查与安全

健康检查监听器默认为 loopback，并暴露以下端点：

~~~text
GET /health/live
GET /health/ready
GET /health/startup
~~~

成功的检查返回 HTTP 200 和 JSON；失败的检查返回相同结构和 HTTP 503。Readiness 要求存在可用的子进程、至少一个符合条件的验证器，并且每个启用的必需验证器都具备准入资格。已禁用的必需验证器不会被计为失败。健康检查和准入端点与公共 DERP 表面相互分离。

容器以 UID/GID 10001 运行，使用只读根文件系统，不挂载 TUN 设备，也不使用 CAP_NET_ADMIN。只有 `/data` 和由所有者控制的 `/run/multiderp` tmpfs 需要可写。请保护验证器状态和 secret 文件：状态中包含 Tailnet 节点身份，因此服务器运维者也就被信任可以持有该身份。需要更强隔离的 Tailnet 所有者还应额外应用自己的 Tailscale Grants/ACL 策略。
在 remove 操作进行期间，守护进程会在配置文件所在目录旁写入 `.multiderp-remove-operation.yaml`。这是一个私有且不包含 secret 的恢复日志。启动时，系统会在启动任何验证器或监听器之前完成或拒绝该日志；不要通过删除它来绕过恢复错误。

服务器端加固只限制验证器进程的能力，并要求 DERP 运维者保护其状态目录。还需要由控制平面强制隔离的 Tailnet 所有者，应使用自己的 Tailscale Grants（或经过明确审查的旧版 ACL），并配合专用验证器标签，例如 `tag:multiderp-verifier`。请同时确认该标签无法对其他设备发起应用访问，并确认控制平面仍会公开 `WhoIsNodeKey` 必须识别的节点。缺少应用访问权限并不一定意味着节点对控制平面查询不可见。MultiDERP 不会解析、创建、上传或重新加载 Tailnet 策略。

## 开发

固定的上游依赖版本是 tailscale.com v1.102.3；Dockerfile 使用相同的模块版本构建 multiderp 和上游 derper。不要脱离加固兼容性矩阵和发布集成测试单独升级它。

启用 Go 的自动工具链选择后：

~~~text
multiderp version
go test ./...
go build ./cmd/multiderp
go build tailscale.com/cmd/derper
~~~

CI/测试边界使用伪造验证器，不需要真实的 Tailscale 账户。真实的 Web/OAuth/auth-key 注册、公共 DERP 升级行为、反向代理转发和 UDP STUN 可达性，仍然属于目标部署中的受控发布测试。

GitHub Actions 会在每次 push 和 pull request 上运行相互独立的检查：

- 在 Linux 上运行 Go 单元测试和 `go vet`；
- 使用 `go test -race ./...` 在 Linux 上进行竞态检测；
- 根据 digest 固定的输入构建 CGO-disabled Linux amd64 版本的 MultiDERP 和上游 derper；
- 对两个二进制文件执行 CGO-disabled Windows amd64 构建；
- 根据 [release-manifest.yaml](release-manifest.yaml) 中 digest 固定的输入构建 Docker 镜像。

该 manifest 记录确切的 `tailscale.com` commit 和 Linux 镜像 digest；模块版本仍固定在 `go.mod` 中，任何升级前都必须结合 [HARDENING-COMPATIBILITY.md](HARDENING-COMPATIBILITY.md) 一起审查。
