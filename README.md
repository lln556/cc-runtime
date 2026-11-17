# cc-runtime

在一台 Mac 上运行多台 Linux 开发机，给 Claude Code 这类编码代理一个可长期使用、彼此隔离的开发环境。

- **每台开发机是一台独立的机器**：自己的主机名、`machine-id`、SSH 主机密钥和登录状态，重建容器后身份不变。
- **每台开发机走自己的出口**：开发机只连私网，所有出站流量经过它专属的 Mihomo 透明网关，没有直连回退，DNS 也不能绕过。
- **和 Mac 共用同一份工作区**：源码实时双向同步，依赖和构建产物按平台分开（Mac 一套、Linux 一套），git 数据只有一份，两边都能提交。
- **一个命令管理全部**：`cc-runtime` 负责启动、同步、git 数据、去重、定时任务和状态面板。

## 架构

```
            Mac                                          私网 172.30.0.0/24
  ┌──────────────────────────┐            ┌──────────────────────────────────────┐
  │ workspace ⇄ linux-workspace ───────────► dev-pennsylvania ──► proxy-pennsylvania ──► 出口 A  │
  │  (Mutagen)    git store  │  bind mount │ dev-california ──► proxy-california ► 出口 B │
  └──────────────────────────┘            │ tailscale ──SSH──► dev-pennsylvania      │
                                          └──────────────────────────────────────┘
```

- [架构](docs/architecture.md)：服务组成、网络与网关、开发机身份、镜像。
- [工作区](docs/workspace.md)：同步副本、依赖隔离、git 数据外置、去重、定时任务，以及为什么这样设计。
- [代理网关配置](proxy/README.md)：网关配置的模板与修改流程。

## 要求

- macOS，OrbStack 或 Docker Desktop。Compose 需要支持卷的 `subpath`（已在 Docker 29.4 / Compose 5.1 上验证）。
- [Mutagen](https://mutagen.io) 0.18 以上，守护进程注册为登录后启动：`mutagen daemon register && mutagen daemon start`。
- [Go](https://go.dev/dl/) 1.26 以上，只在安装 CLI 时需要；装好的 `cc-runtime` 是单个二进制文件。
- 去重需要支持克隆的文件系统（APFS；Linux 上为 Btrfs、XFS 等）。

## 开始

1. **网关配置。** 每个网关一份实际配置，由模板派生：

   ```bash
   cp proxy/config.example.yaml proxy/config-pennsylvania.yaml
   cp proxy/config.example.yaml proxy/config-california.yaml
   chmod 600 proxy/config*.yaml
   ```

   按 [`proxy/README.md`](proxy/README.md) 填入节点和出口。这是原生 Mihomo YAML，网关会拒绝启动仍含模板占位符的配置。

2. **安装 CLI。** `scripts/install.sh` 编译 `cc-runtime` 并装到 `~/.local/bin`（确保它在 `PATH` 里）。二进制记录了本仓库的位置，在任何目录下都能运行；更新代码后重新运行这个脚本即可。

3. **环境变量。** `cp .env.example .env`，填写 `PASSWORD`（开发机的 SSH/VNC 密码）和三个路径：`WORKSPACE_DIR`（Mac 的工作区）、`RUNTIME_DIR`（Linux 副本和 git 数据，放在工作区之外）、`DOWNLOADS_DIR`（Mac 的下载目录）。工作区、git 数据和下载目录在开发机里挂在和 Mac 相同的路径上。可选的 `HOST_SCREENSHOTS_PATH` 把截图目录也以相同路径只读挂进开发机。

4. **工作区同步。**

   ```bash
   mkdir -p "$RUNTIME_DIR/linux-workspace" "$RUNTIME_DIR/git"
   cc-runtime check ignores      # 先检查忽略名单，按提示补充 sync-ignores.local
   cc-runtime sync create
   cc-runtime sync status        # 等首次同步完成
   ```

   `linux-workspace` 里会有大量 Linux 依赖，建议从 Spotlight 和 Time Machine 中排除；`git` 目录保存所有仓库的历史，要保留在备份里。

5. **迁出 git 数据并安装定时任务。**

   ```bash
   cc-runtime git externalize --all
   cc-runtime schedule install --all
   ```

   launchd 任务直接调用 `~/.local/bin/cc-runtime`，不依赖仓库里的任何环境。

6. **启动。**

   ```bash
   cc-runtime up --pull
   cc-runtime status
   ```

   `*-network-init` 设置完路由后退出，`Exited (0)` 是正常状态。

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
| `cc-runtime status [--git] [--watch[=秒]]` | 状态面板：同步会话、定时任务、git 数据、磁盘、容器 |
| `cc-runtime up [服务…] [--pull]` | 启动服务；先准备家目录、身份和缓存 |
| `cc-runtime compose <参数…>` | 本部署的 `docker compose` |
| `cc-runtime sync status \| compare [--content] \| ignores \| create` | 同步会话 |
| `cc-runtime git status \| externalize \| restore` | 仓库的 git 数据 |
| `cc-runtime dedupe [--dry-run]` | 去重一次 |
| `cc-runtime schedule list \| install \| uninstall` | launchd 定时任务 |
| `cc-runtime job run \| log <任务>` | 立即运行任务，或查看运行记录 |
| `cc-runtime seed-home <源> <目标>` | 用现有家目录初始化新开发机的家目录 |
| `cc-runtime check ignores \| proxy` | 检查忽略名单、网关配置与模板是否一致 |

使用工作区的约定（同一时间只在一侧做 git 写操作、复制项目用 `git clone` 等）见 [工作区](docs/workspace.md#git-数据外置)。

## 目录

```text
compose.yaml          服务、网络、挂载
Dockerfile            开发镜像（构建上下文只有 scripts/entrypoint.sh 和 shell/）
shell/                镜像内的 shell 配置
cmd/cc-runtime/       CLI 入口
internal/             CLI 的各个部分（同步、git 数据、去重、定时任务、部署等）和测试
scripts/install.sh    编译并安装 CLI
scripts/entrypoint.sh 开发机启动脚本：SSH、VNC、桌面
scripts/              网关和 network-init 容器内使用的脚本
sync-ignores          同步忽略名单（所有部署共用）
proxy/                网关配置模板、DNS 配置；实际配置（config-pennsylvania.yaml、config-california.yaml）不入库
tailscale/config/     Tailscale Serve 配置；登录状态在 tailscale/state/，不入库
dev-runtimes/         各开发机的家目录、SSH 主机密钥、machine-id，不入库
docs/                 架构文档；docs/local/ 为本机私有笔记，不入库
.env                  密码和路径，不入库（示例见 .env.example）
sync-ignores.local    本机的同步忽略条目，不入库
```

## 维护

- **改网关配置**：先改 `proxy/config.example.yaml`，同步到每份实际配置，运行 `cc-runtime check proxy`，再重建对应网关（`cc-runtime compose up -d --no-deps --force-recreate proxy-pennsylvania`）。
- **改镜像或 Compose 后重建开发机**：`cc-runtime up <开发机> --pull --force-recreate`。重建会中断其中的会话，身份和家目录保留。
- **增加一台开发机**：在 `compose.yaml` 里加开发机、网关和 `network-init` 三个服务，网关配置加一份；用 `cc-runtime seed-home` 初始化 `dev-runtimes/<名字>/home`，然后 `cc-runtime up <名字>`。
- **备份**：`dev-runtimes/`（身份和家目录）、`tailscale/state/`（同一设备身份不要在两处同时使用）、`RUNTIME_DIR/git`（所有仓库的历史）。`linux-workspace` 可以随时从 Mac 的工作区重新同步出来。
- **网段**固定为 `172.30.0.0/24`。和其他网络冲突时，要同时改 `compose.yaml`、`scripts/` 下的两个脚本、`proxy/resolv*.conf` 和网关配置里的 tunnel 目标。

## 测试

```bash
go test ./...
CC_RUNTIME_DOCKER_TEST=1 go test ./internal/deploy   # 加上 Docker 集成测试
```

去重的 `FIDEDUPERANGE` 测试需要 Btrfs/XFS 等文件系统，`clonefile` 测试只在 macOS 上运行，条件不满足时自动跳过。
