[English](README.md) | 简体中文

# UniDERP v2

UniDERP 让自建 Tailscale DERP 中继服务多个独立 tailnet。单个主控管理平台账号、只读设备身份、服务器资源和共享授权；每个中继运行 patched `derper`，根据有期限的本地策略缓存核验设备公钥，并按 tailnet 调度流量。客户端使用标准 DERP 协议，尾网管理员手动配置 DERP map。

主控本机也可提供中继，注册和授权规则与成员节点相同。成员只保存节点身份及有效策略；OAuth 凭据和账号数据库留在主控。Tailscale 继续负责对端身份、网络策略和 WireGuard 加密。

当前分支处于 v2 发布准备阶段，示例使用本地构建镜像。两个独立真实 tailnet 的只读 OAuth 和原版 Tailscale 应用已有隔离验收结果，包括共享确认前拒绝连接，以及通过主控和成员中继的公网 external 与手动证书 passthrough 文件传输；标准 Dockerfile 使用正常构建缓存的完整构建已通过。自动证书签发仍未验证；不使用依赖缓存的构建在依赖下载时遇到 EOF 错误。

## 构建

使用 Go 1.27.1 和固定的 `tailscale.com v1.102.3`。需要同时构建两个程序，原版 `derper` 不提供本项目的策略接口：

```sh
CGO_ENABLED=0 go build -trimpath -o uniderp ./cmd/uniderp
go run ./scripts/build-derper -out derper
UNIDERP_TEST_DERPER="$PWD/derper" go test ./...
go vet ./...
docker build --build-arg UNIDERP_VERSION=v2-local \
  --build-arg UNIDERP_COMMIT="$(git rev-parse HEAD)" -t uniderp:v2-local .
```

构建器核对上游版本、commit 和 module checksum，验证并应用[补丁](patches/tailscale/uniderp.patch)，再运行 patched 包测试。[release-manifest.yaml](release-manifest.yaml) 记录上游 commit、补丁摘要和基础镜像摘要。`uniderp version` 显示产品版本、commit、上游版本和 patch ID；未传入发布元数据的本地构建显示开发值。

## 部署主控

以下命令用于 Linux Docker 主机。准备公网域名和 TCP 443 上的可信 TLS，开放 UDP 3478 STUN。出站 HTTPS 需要访问 Tailscale OAuth/设备 API 和成员域名。节点域名通过 HTTPS 443 核验；使用私有地址需由部署者明确设置主控 `allowed_node_cidrs`。

```sh
mkdir -p data
cp config.example.yaml data/config.yaml
# 启动前修改 server.hostname 和部署配置。
sudo chown -R 10001:10001 data
sudo chmod 700 data
sudo chmod 600 data/config.yaml
docker compose -f docker-compose.example.yaml up -d
```

[config.example.yaml](config.example.yaml) 启用主控，使用 `/data/controller.sqlite`、`/data/controller.key` 和 `/data/node`。镜像以 UID/GID 10001 运行，根文件系统只读，`/run/uniderp` 为私有 tmpfs。将整个 `/data` 持久化并允许该 UID 写入，保证密钥、SQLite WAL 和节点状态一起保留。admin socket 和 health 监听保持本地访问。

打开 `https://你的主控域名/manage/`。尚未配置管理员时，页面会让你设置首个管理员的用户名、密码（12–72 字节）及确认密码，提交后自动登录。之后访问显示正常登录页。

自动化部署也可以通过本地 admin socket 初始化：用受保护的编辑器或 secret 工具创建 `/data/admin-password`，写入 12–72 字节密码，仅允许 UID 10001 读取。不要把内容写进命令参数或日志。初始化后删除临时文件：

```sh
docker exec uniderp uniderp controller init \
  --username admin --password-file /data/admin-password
```

管理员创建成员账号，并在设置页调整身份保留期和控制保留期。平台密码与尾网凭据独立；尾网 API 故障不会阻止平台登录。

### TLS 与代理

`tls_mode: external` 由反向代理终止 TLS，derper 接收 HTTP。`tls_mode: passthrough` 由 derper 自己终止 TLS 并加载证书，客户端可直连，或通过透传 TLS 的 TCP 代理连接。这里的 passthrough 指上游代理将 TLS 传给 derper。

external 示例只把明文后端暴露在 `127.0.0.1:3377`。主机反向代理负责 TLS、HTTP/1.1 upgrade 和主控长连接响应。所有路径转发到后端，patched derper 将 `/manage/`、`/api/v1/`、`/cluster/v1/` 路由到内部管理服务。已有 Nginx TLS server 可采用以下 location 设置：

```nginx
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

代理仍需有效证书与 DNS。代理若运行在另一容器，应建立私有后端网络；另一容器的 loopback 不等于主机地址。STUN 直接使用 UDP 3478，不走 HTTP 代理。

直接使用 Let's Encrypt TLS 时，采用 [docker-compose.letsencrypt.example.yaml](docker-compose.letsencrypt.example.yaml)，并设置：

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

该示例公开 TCP 80/443，授予 `NET_BIND_SERVICE`。实际签发和生产代理仍需在部署环境验证。

使用已有证书时设置 `cert_mode: manual`。在 `cert_dir` 中提供 PEM 格式的 `relay.example.com.crt`（站点证书及所需中间证书链）和匹配的 `relay.example.com.key`；证书必须覆盖 `server.hostname`。目录允许 UID 10001 访问，私钥仅允许该服务身份读取。手动证书在 derper 启动时加载，更换后需重启节点。

手动 TLS 可使用非 443 后端端口：

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

例如将主机 TCP 3489 映射到容器 TCP 3377，就能在 3489 上提供直连 TLS。在管理页面把该节点资源的公开 DERP TCP 端口设为 3489；公开 STUN UDP 端口单独配置。两项默认 443/3478，表示宿主机映射或代理入口，不是容器 listener；导出 map 和独立探测均使用已保存的值。所有者和管理员可修改，共享者只读。修改后需重新导出并更新各尾网的 map。主控仍通过公网 HTTPS 443 验证节点域名，因此该入口也要可达。主机和云防火墙都需放行映射后的端口，并服务 DNS 公布的各地址族。Let's Encrypt 模式要求配置中的 DERP listener 使用 443，并保证 ACME 入口可达，不能只改端口就沿用该模式。

手动 TLS 要求握手 SNI 与 `server.hostname` 匹配。检查回环后端时，可保留域名和正常证书验证：

```sh
curl --resolve relay.example.com:3489:127.0.0.1 \
  https://relay.example.com:3489/derp/probe
```

HTTP 反代也可以终止公网 TLS，再与 passthrough 后端建立另一条 TLS 连接；这与 TCP 代理原样透传 TLS 不同。在前面的 Nginx location 中替换 HTTP `proxy_pass`，并加入：

```nginx
proxy_pass https://127.0.0.1:3489;
proxy_ssl_server_name on;
proxy_ssl_name relay.example.com;
proxy_ssl_verify on;
proxy_ssl_trusted_certificate /etc/ssl/certs/ca-certificates.crt;
proxy_ssl_verify_depth 3;
```

CA bundle 路径必须存在于代理所在的容器或主机。Nginx 默认不发送后端 SNI、不验证后端证书，验证深度默认是 1；有效但较长的证书链可能报 `certificate chain too long`。应根据实际证书链设置足够的深度，示例使用 3，保留证书验证。`proxy_ssl_*` 仅用于 HTTPS 后端；external 的 HTTP 后端不需要这些设置。参见 [Nginx 后端 TLS 指令](https://nginx.org/en/docs/http/ngx_http_proxy_module.html#proxy_ssl_verify_depth)。

## 加入成员中继

在成员主机将 [config.node.example.yaml](config.node.example.yaml) 复制到 `node-data/config.yaml`，设置 `node.controller_url` 为主控 HTTPS origin，`server.hostname` 为成员自己的公网域名。按主控相同方式设置 `node-data` 的 UID 10001 和严格权限，启动 [docker-compose.node.example.yaml](docker-compose.node.example.yaml)。配置成员 TLS/代理和 STUN。各 Compose 示例用于各自的主机；同机部署时自行调整端口。

提供者在“我的服务器”中为准确域名创建资源，取得有效 30 分钟的一次性注册码。在成员主机写入受保护的 `/data/enrollment-code`：

```sh
docker exec uniderp-node uniderp node enroll \
  --controller https://control.example.com --code-file /data/enrollment-code
```

成功后删除临时文件。注册同时验证节点私钥持有和 HTTPS 域名控制权。持久保留 `/data/node/node.key`、注册记录与策略状态。注册主控本机中继时，同样先创建资源，再在 `uniderp` 容器执行该命令，使用其自身主控 HTTPS origin。管理员身份不会自动取得中继权限。

## 绑定尾网与共享授权

1. 在“我的 tailnet”输入 Tailscale General 设置中的规范 `T...` Tailnet ID，以及只配置 `devices:core:read` 的 OAuth client ID/secret。UniDERP 请求该只读 scope 并同步设备公钥。成功读取不能证明原始 OAuth client 没有其他权限，所有者需检查原始配置。参见 [Tailscale OAuth clients](https://tailscale.com/docs/features/oauth-clients) 和 [trust credential scopes](https://tailscale.com/docs/reference/trust-credentials)。
2. 在服务器目录为自己的 tailnet 申请使用中继。提供者批准后，申请人还需确认生效；未确认不能放行设备。提供者自己的尾网直接生效。
3. 导出该尾网 DERP map，将 `Regions` 合并到现有 Tailscale policy 的 `derpMap.Regions`。保留原有 ACL/grants、其他区域和默认 DERP 设置，并检查 900–999 的区域 ID 是否冲突。UniDERP 不自动修改 policy 或分发客户端配置。参见 [自定义 DERP 服务器](https://tailscale.com/docs/reference/derp-servers)。

map 公布仍有效的授权资源，使用各资源保存的公开 DERP TCP 和 STUN UDP 端口，默认分别为 443 和 3478。发现与授权分开：旧 map 条目不会赋予设备公钥权限。标准协议面向原版客户端，但本地 DERP library 测试不能替代真实尾网中的原版应用验证。

## 带宽与失效期限

提供者设置载荷预算和 owner/shared 权重，初始为 100 Mbps、8:2。持续争用时 owner 组约占 80%，shared 组约占 20%；空闲容量可借用。shared 内按各 tailnet 权重分配，不因设备或连接数量增加份额。可选的组/尾网上限限制借用。修改规则无需重新确认共享授权。

RX、TX 分别调度字节、分别使用预算。统计为中继载荷，不含传输开销；不能把 RX+TX 合计视为一个预算。包长、burst 和采样窗口会影响短期读数。这不是宿主机总带宽预留或公网吞吐保证。

设备的有效期限取身份保留期、主控联系保留期、显式 grant 到期和真实设备 key 到期中的最早值。身份期限从最后一次完整成功 API 刷新计算，失败或部分刷新不续期；控制期限从最后一次成功主控 heartbeat 计算。两项初始设置均为 24 小时，管理员可调整。已经缓存的绝对期限不会因失联或重启而重新计时。

在线撤销在节点应用新策略后关闭既有连接。界面分别显示主控 desired、节点 received、derper applied；收到版本不等于 applied ACK。离线中继只能使用到原缓存期限，主控删除资源无法立即通知离线进程。

## 运维与恢复

管理页区分 heartbeat/应用状态、设备与授权期限、区间流量速率以及独立 DERP/STUN 探测。探测有自己的观察时间，不能证明所有客户端路径。共享者仅查看自己尾网用量，提供者和平台管理员有更广的资源视图。事件与审计按相关资源过滤，管理员代操作记录真实 actor。

删除服务器或改域名前先暂停。在线节点必须先 ACK 空策略。改域名保留节点身份，需重新证明 HTTPS 域名控制权，并保持暂停直到显式启用。同时更新该中继的 hostname 配置、DNS 和 TLS/代理；listener/TLS/hostname 修改在 config reload 后还需重启 daemon。保持已注册节点使用的主控 origin 可访问，修改中继域名不会迁移主控 origin。节点身份被复制时，先停止重复进程，再在管理页恢复实例。普通重启可等待前一个 90 秒实例租约到期；不要靠删除密钥绕过冲突。

```sh
docker exec uniderp uniderp config reload
docker exec uniderp uniderp derp restart
docker stop --time 30 uniderp
```

停服务后备份配置及整个持久目录，或使用包含 WAL 一致性的 SQLite 备份。主控数据库必须与 `controller.key` 配套，同时保留节点私钥/注册、策略及 `.watermark`、derper 持久身份和证书。缺少对应加密密钥无法恢复 OAuth 密文。排障前先保留这些文件，不要换空库或删除节点状态。本地管理员恢复通过受保护的 admin socket 执行 `controller recover --user-id ID --password-file PATH`。

## 从 v1 迁移

停止旧 MultiDERP/UniDERP，备份完整配置、数据目录和准确镜像/版本。创建独立 v2 数据目录及 `version: 2` 配置，初始化账号，重新提交只读 OAuth 凭据，注册中继域名，重建申请/批准/确认。导出并检查新 DERP map 后再更新各尾网 policy。

version 1 配置会以明确迁移错误停止。旧 verifier state 不能转换为 OAuth secret 或共享授权，也不会自动删除。回退时停止 v2，恢复旧镜像及原配置/数据，并审核恢复 policy。不要让旧程序打开 v2 SQLite 数据库。

## 发布与验证边界

CI 构建 patched derper 后运行集成测试，分别对 patched 上游转发和主控/集群运行 Linux race 检查。本地已有权限 API/浏览器流程、可信本地 TLS/STUN、DERP library 转发/撤销、确定性和受控字节调度、真实 Linux race 证据。公网 DNS、真实 OAuth、原版应用和 WAN 行为仍是独立验收项。

镜像 workflow 接受稳定 `vX.Y.Z` 和 `v2.0.0-alpha.1` 等预发布 tag。预发布只生成明确的版本镜像 tag，不更新 `latest`；稳定 tag 保留既有 `latest` 行为。建 tag、push、发布 release、远端仓库改名及 GHCR 迁移需分别授权。本地分支尚未发布 v2 镜像。

UniDERP 使用 [GNU GPL v3](LICENSE)。信任边界与漏洞报告见 [SECURITY.md](SECURITY.md)，patched 上游许可见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)，发布历史见 [CHANGELOG.md](CHANGELOG.md)。
