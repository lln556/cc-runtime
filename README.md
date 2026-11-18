# cc-runtime

在一台 Mac 上运行多台 Linux 开发机，给 Claude Code 这类编码代理一个可长期使用、彼此隔离的开发环境。

- **每台开发机是一台独立的机器**：自己的主机名、`machine-id`、SSH 主机密钥和登录状态，重建容器后身份不变。
- **每台开发机走自己的出口**：开发机只连私网，所有出站流量经过它专属的 Mihomo 透明网关，没有直连回退，DNS 也不能绕过。
- **命令在 Mac 上执行**：开发机里只跑 Claude Code，它的 Bash 命令经 remote-shell 在 Mac 的登录会话里执行，用 Mac 的工具链和依赖；Mac 的工作区直接挂进开发机，只有一份。
- **一个命令管理全部**：`cc-runtime` 负责启动、remote-shell 服务、定时任务和状态面板。

## 架构

```
            Mac                                          私网 172.30.0.0/24
  ┌────────────────────────────┐          ┌──────────────────────────────────────────────┐
  │ workspace ─────────────────┼─ mount ─►│ dev-pennsylvania ──► proxy-pennsylvania ──► 出口 A │
  │ remote-shell 服务 ◄────────┼─ tunnel ─│ dev-california   ──► proxy-california   ──► 出口 B │
  │   └─ zsh 执行 Bash 命令    │          │ tailscale ──SSH──► dev-pennsylvania            │
  └────────────────────────────┘          └──────────────────────────────────────────────┘
```

- [架构](docs/architecture.md)：服务组成、网络与网关、开发机身份、镜像。
- [remote-shell](docs/remote-shell.md)：开发机里 Claude Code 的 Bash 命令怎么在 Mac 上执行。
- [代理网关配置](proxy/README.md)：网关配置的模板与修改流程。

## 要求

- macOS，OrbStack 或 Docker Desktop。Compose 需要支持卷的 `subpath`（已在 Docker 29.4 / Compose 5.1 上验证）。
- [Go](https://go.dev/dl/) 1.26 以上，只在安装 CLI 时需要；装好的 `cc-runtime` 是单个二进制文件。

## 开始

1. **网关配置。** 每个网关一份实际配置，由模板派生：

   ```bash
   cp proxy/config.example.yaml proxy/config-pennsylvania.yaml
   cp proxy/config.example.yaml proxy/config-california.yaml
   chmod 600 proxy/config*.yaml
   ```

   按 [`proxy/README.md`](proxy/README.md) 填入节点和出口。这是原生 Mihomo YAML，网关会拒绝启动仍含模板占位符的配置。

2. **安装 CLI。** `scripts/install.sh` 编译 `cc-runtime` 并装到 `~/.local/bin`（确保它在 `PATH` 里）。二进制记录了本仓库的位置，在任何目录下都能运行；更新代码后重新运行这个脚本即可。

3. **环境变量。** `cp .env.example .env`，填写 `PASSWORD`（开发机的 SSH/VNC 密码）和三个路径：`WORKSPACE_DIR`（Mac 的工作区）、`RUNTIME_DIR`（remote-shell 的机器目录、开发机用的客户端、日志和状态，放在工作区之外）、`DOWNLOADS_DIR`（Mac 的下载目录）。工作区和下载目录在开发机里挂在和 Mac 相同的路径上。可选的 `HOST_SCREENSHOTS_PATH` 把截图目录也以相同路径只读挂进开发机。

4. **remote-shell 服务。**

   ```bash
   cc-runtime remote-shell install   # 写入 LaunchAgent 并启动
   ```

5. **启动。**

   ```bash
   cc-runtime up --pull
   cc-runtime status
   ```

   `*-network-init` 设置完路由后退出，`Exited (0)` 是正常状态。

6. **Claude Code。** 按 [remote-shell](docs/remote-shell.md#和-claude-code-的衔接) 配置开发机的 Claude Code，然后 `cc-runtime check remote-shell`。

7. **Tailscale（可选）。** `cc-runtime compose logs tailscale` 里有登录链接；登录后在管理后台为该设备关闭密钥过期。

## 访问

| 开发机 | SSH | VNC | noVNC |
|---|---|---|---|
| `dev-pennsylvania` | `ssh -p 2222 dev@127.0.0.1` | `127.0.0.1:5901` | `http://127.0.0.1:6080/vnc.html` |
| `dev-california` | `ssh -p 2223 dev@127.0.0.1` | `127.0.0.1:5902` | `http://127.0.0.1:6081/vnc.html` |

- 经 Tailscale：`ssh dev@cc-dev`（设备名由 `TS_HOSTNAME` 设置），只开放 SSH，其他端口用 SSH 端口转发。
- 从开发机到 tailnet：两台开发机都能按 IP（`100.x.y.z`）访问 tailnet 设备的 TCP 端口，经 `tailscale` 容器出去，不支持 MagicDNS 名字。
- 从开发机到 Mac：Mac 开启「远程登录」后，`ssh -p 10023 <Mac 用户名>@<网关 IP>`（`dev-pennsylvania` 为 `172.30.0.2`，`dev-california` 为 `172.30.0.4`）。
- 网关控制 API：`127.0.0.1:19090`（`proxy-pennsylvania`）、`127.0.0.1:19091`（`proxy-california`），密钥为 `PASSWORD`。

## 日常使用

| 命令 | 作用 |
|---|---|
| `cc-runtime status [--watch[=秒]]` | 状态面板：remote-shell、定时任务、磁盘、容器 |
| `cc-runtime up [服务…] [--pull]` | 启动服务；先准备家目录、身份、缓存和 remote-shell 的机器目录 |
| `cc-runtime compose <参数…>` | 本部署的 `docker compose` |
| `cc-runtime schedule list \| install \| uninstall` | launchd 定时任务 |
| `cc-runtime job run \| log <任务>` | 立即运行任务，或查看运行记录 |
| `cc-runtime seed-home <源> <目标>` | 用现有家目录初始化新开发机的家目录 |
| `cc-runtime remote-shell status \| install \| uninstall \| restart [--force]` | Mac 上的 remote-shell 服务 |
| `cc-runtime check proxy \| remote-shell` | 检查网关配置与模板是否一致、remote-shell 端到端可用 |

Claude Code 要在工作区或下载目录里启动：开发机的 `/home/dev`、`/tmp` 在 Mac 上不存在，在那里启动时 Bash 命令会被拒绝。

## 目录

```text
compose.yaml          服务、网络、挂载
Dockerfile            开发镜像（构建上下文只有 scripts/entrypoint.sh 和 shell/）
shell/                镜像内的 shell 配置
cmd/cc-runtime/       CLI 入口
internal/             CLI 的各个部分（部署、remote-shell、定时任务、状态面板等）和测试
scripts/install.sh    编译并安装 CLI
scripts/entrypoint.sh 开发机启动脚本：SSH、VNC、桌面
scripts/              网关和 network-init 容器内使用的脚本
proxy/                网关配置模板、DNS 配置；实际配置（config-pennsylvania.yaml、config-california.yaml）不入库
tailscale/config/     Tailscale Serve 配置；登录状态在 tailscale/state/，不入库
dev-runtimes/         各开发机的家目录、SSH 主机密钥、machine-id，不入库
docs/                 架构文档；docs/local/ 为本机私有笔记，不入库
.env                  密码和路径，不入库（示例见 .env.example）
```

## 维护

- **改网关配置**：先改 `proxy/config.example.yaml`，同步到每份实际配置，运行 `cc-runtime check proxy`，再重建对应网关（`cc-runtime compose up -d --no-deps --force-recreate proxy-pennsylvania`）。
- **改镜像或 Compose 后重建开发机**：`cc-runtime up <开发机> --pull --force-recreate`。重建会中断其中的会话，身份和家目录保留。
- **改 remote-shell**：在 Mac 上运行 `scripts/install.sh`，它同时交叉编译开发机用的客户端并重启服务；然后 `cc-runtime check remote-shell`。
- **增加一台开发机**：在 `compose.yaml` 里加开发机、网关和 `network-init` 三个服务，网关配置加一份；用 `cc-runtime seed-home` 初始化 `dev-runtimes/<名字>/home`，然后 `cc-runtime up <名字>`。
- **备份**：`dev-runtimes/`（身份和家目录）、`tailscale/state/`（同一设备身份不要在两处同时使用）。`RUNTIME_DIR` 里没有不可再生的数据：机器目录和 token 由 `cc-runtime up` 重新生成。
- **网段**固定为 `172.30.0.0/24`。和其他网络冲突时，要同时改 `compose.yaml`、`scripts/` 下的两个脚本、`proxy/resolv*.conf` 和网关配置里的 tunnel 目标。

## 测试

```bash
go test ./...
CC_RUNTIME_DOCKER_TEST=1 go test ./internal/deploy   # 加上 Docker 集成测试
```

