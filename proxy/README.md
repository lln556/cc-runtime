# 代理网关配置

本目录存放 Mihomo 网关配置。共有内容只维护在模板里，每个网关的实际配置由模板派生。

| 文件 | 网关容器 | 私网 IP | 最终出口 | 提交 Git |
|---|---|---|---|---|
| `config.example.yaml` | — | — | 共有模板（占位符） | 是 |
| `config-pennsylvania.yaml` | `proxy-pennsylvania` | `172.30.0.2` | `residential` 家宽 HTTP 代理 | 否 |
| `config-california.yaml` | `proxy-california` | `172.30.0.4` | `california` 加州 VLESS Reality 节点 | 否 |
| `resolv.conf`、`resolv-california.conf` | — | — | 开发机的 DNS 指向各自网关 | 是 |

仓库是公开的。实际配置含机场、家宽和节点凭据，已加入 `.gitignore`，不要提交，也不要把真实值（包括 VPS IP）写进模板。

## 修改流程

**任何修改都先改 `config.example.yaml`，再把同样的改动同步到 `config-pennsylvania.yaml` 和 `config-california.yaml`。** 然后执行：

```bash
cc-runtime check proxy
```

它会对比每份实际配置与模板在 `GATEWAY-SPECIFIC` 块之外的内容，不一致时打印差异（凭据行会隐藏）并以非 0 退出。

只属于某个网关的改动（例如加州的上游组顺序），直接改该网关配置里对应的 `GATEWAY-SPECIFIC` 块；如果新增了一类网关特有内容，先在模板里加一个 `GATEWAY-SPECIFIC` 块并写明各网关的差异。

## 共有部分与网关特有部分

模板中 `# ==== GATEWAY-SPECIFIC: ... ====` 与 `# ==== END GATEWAY-SPECIFIC ====` 之间是网关特有内容，其余部分在所有实际配置中必须逐字相同，只有以下值例外：

| 模板中的值 | 实际配置中填写 |
|---|---|
| `airport` 整个节点（`airport.example.com`、`REPLACE_AIRPORT_PASSWORD`） | 机场订阅中的一个完整节点，名称保留 `airport` |
| `IP-CIDR,192.0.2.10/32,DIRECT,no-resolve` | 需要绕过代理链直连的主机 IP（例如自建出口节点的 VPS） |

共有规则和 DNS 使用 `egress`、`airport`、`DIRECT` 和 `tailnet`，不直接引用网关特有的出口节点名。每个网关在自己的 `egress groups` 块里把 `egress` 指向本网关的出口节点，所以 `rules` 与 `dns` 两段在派生配置中保持相同。

| 块 | `config-pennsylvania.yaml`（主网关） | `config-california.yaml` |
|---|---|---|
| `egress proxy` | `residential`（HTTP），`dialer-proxy: residential-upstream` | `california`（VLESS Reality + Vision），`server` 写 IP，`dialer-proxy: california-upstream` |
| `egress groups` | `egress → residential`；`residential-upstream`：`default`（经 airport）/ `weimob`（直连） | `egress → california`；`california-upstream`：`airport`（默认）/ `DIRECT` |
| `listeners` | 无 | `mac-mixed`：容器内 `0.0.0.0:7890`，宿主机发布为 `127.0.0.1:17890` |
| `tunnels` | 到 Mac 的 SSH `172.30.0.2:10023`；`dev-pennsylvania`（`172.30.0.10`）的 SSH 入站 | 到 Mac 的 SSH `172.30.0.4:10023`；`dev-california`（`172.30.0.11`）的 SSH 入站 |

## 流量与 DNS

两个网关的分流完全相同：

- Tailscale MagicDNS：`nameserver-policy` 的 `'+.ts.net': ['tcp://100.100.100.100:53#tailnet']` 经 sidecar 的 SOCKS5 查询其自身的 MagicDNS，不依赖 Mac 的 Tailscale 或系统 DNS。使用通用后缀，不在模板中填写个人 tailnet 名称。
- Tailnet TCP：先匹配 `DOMAIN-SUFFIX,ts.net,tailnet`，再匹配 `IP-CIDR,100.64.0.0/10,tailnet,no-resolve`，均经 `172.30.0.3:1055`；域名规则也覆盖 HTTP/SOCKS 代理传入的域名。其他公网域名仍按后续规则分流，不交给 sidecar。
- 公司、内网和直连域名：`DIRECT`，经 Docker 出口走宿主机网络；DNS 用系统解析器（Docker → 宿主机）。
- 依赖与工具链下载（PyPI、npm、GitHub、Rust 等）：`airport`。
- Anthropic、Datadog 及其他所有流量（`MATCH`）：`egress`。
- 其他域名的 DNS：DoH `1.1.1.1` 经 `egress` 发出。主网关为 `airport → residential`，加州网关为 `airport → california`。
- 代理节点域名用 `system` 解析（启动引导）；出口节点优先直接写 IP。

## 加州网关

- 当前 `compose.yaml` 的宿主机 HTTP/SOCKS 代理：`127.0.0.1:17890`；加州控制 API：`127.0.0.1:19090`（密钥同 `.env` 的 `PASSWORD`）。使用其他 Compose 配置时，先核对对应容器的端口映射，避免重载到其他网关。
- `proxy-entrypoint.sh` 通过 `GATEWAY_IP` / `CLIENT_IP` 环境变量区分网关；`CLIENT_IP: 172.30.0.11` 为 `dev-california`。入口脚本拒绝 Docker 私网访问 7890，dev 容器不能借此绕过透明网关。
- `california-upstream` 默认经 `airport` 连接出口节点，可在面板切到 `DIRECT`。
- 服务端为 Xray 时注意：Xray 26 在 inbound 的 `minClientVer` 为空时会拒绝 Mihomo（表现为 `REALITY authentication failed`），需要把它设为 `1.0.0` 之类的低版本。

## 生效与验证

实际配置以单文件 bind mount 挂入容器。优先原地覆盖写回，保持 inode 不变（例如 `cat /path/to/edited.yaml > proxy/config-california.yaml`），再校验配置并确认宿主机与容器内的 SHA256 相同：

```bash
cc-runtime check proxy
shasum -a 256 proxy/config-california.yaml
docker exec cc-runtime-proxy-california-1 sha256sum /root/.config/mihomo/config.yaml
docker exec cc-runtime-proxy-california-1 /mihomo -t -f /root/.config/mihomo/config.yaml
```

确认端口映射后，对加州控制 API 执行 `PUT /configs?force=true`，请求体为 `{"path":"/root/.config/mihomo/config.yaml"}`，使用 `.env` 的 `PASSWORD` 作为 Bearer 凭据。不要把凭据写入文档、日志或共享命令输出。返回 204 后检查网关日志，并从开发机验证 MagicDNS、tailnet IP 和普通公网访问。热重载无需重启容器，可保留现有 TCP 连接。

编辑器保存可能替换 inode，此时容器仍读旧文件，`restart` 也不能修复挂载。只有挂载内容不一致或热重载失败时，才执行 `cc-runtime compose up -d --no-deps --force-recreate proxy-california`，并等待网关恢复 `healthy`；重建会短暂中断它所服务的开发机连接。不要为网关配置修改重启开发机或 Tailscale sidecar。

只校验语法时：

```bash
docker run --rm -v "$PWD/proxy/config-california.yaml:/c.yaml:ro" \
  --entrypoint /mihomo metacubex/mihomo:latest -t -f /c.yaml
```
