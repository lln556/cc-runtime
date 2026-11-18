# cc-runtime

在一台 Mac 上运行多台 Linux 开发机（`dev-pennsylvania`、`dev-california`），每台有独立身份和出口网关。开发机直接挂载 Mac 的工作区，里面只跑 Claude Code，它的 Bash 命令经 remote-shell 在 Mac 上执行。设计见 `docs/architecture.md` 和 `docs/remote-shell.md`，使用见 `README.md`。

## 常用命令

```bash
go test ./...                # 单元测试
scripts/install.sh           # 编译并安装到 ~/.local/bin（在 Mac 上运行）
cc-runtime status            # 状态面板（在 Mac 上运行）
```

`cc-runtime` 的大部分命令（`up`、`remote-shell`、`schedule`、`status`）要在 Mac 上运行：它们操作 Docker、launchd 和 Mac 上的路径。从开发机里经 `ssh -p 10023 <用户>@<网关 IP>` 到 Mac 执行，Mac 上需要先 `export PATH=$HOME/.local/bin:/opt/homebrew/bin:/usr/local/bin:$PATH`。改了代码要在 Mac 上重新运行 `scripts/install.sh`，launchd 任务用的是装好的二进制。

## 部署约定

- 持久状态优先用宿主机 bind mount，数据直接可见、可备份。例外是 `~/.cache` 所在的外部缓存卷（`.env` 里的 `CACHE_VOLUME` 和各机器的子路径），没有单独要求时不要迁移它。
- 文件按服务归组：Tailscale 配置在 `tailscale/config/`，登录状态在 `tailscale/state/`；每台开发机的家目录、SSH 主机密钥和 machine-id 在 `dev-runtimes/<机器名>/`（`home/`、`ssh-host-keys/`、`machine-id`）。新开发机命名为 `dev-{代号}`。
- 配置和脚本入库；凭据、私钥、运行时状态和家目录不进 Git，也不进 Docker 构建上下文（`.dockerignore` 是白名单，只放行镜像需要的文件）。
- 更换镜像、重建容器或移动存储时，保留 SSH 主机密钥和 Tailscale 状态。移动运行中的状态前先停止对应服务，重启后核对身份（主机密钥指纹、machine-id）。
- remote-shell：所有开发机都启用，挂载写在 `compose.yaml` 里。`scripts/install.sh` 同时交叉编译开发机用的 linux 版到 `RUNTIME_DIR/bin`，并重启 Mac 上的服务（有命令在运行时只提示）。两端不兼容的协议改动要增加 `internal/remoteshell` 的 `ProtocolVersion`。改完用 `cc-runtime check remote-shell` 端到端验证。
- 用 `cc-runtime up` 或 `cc-runtime compose` 启动服务。它们会先准备家目录、身份和缓存；直接 `docker compose up` 创建开发机会跳过这一步，新机器会得到全新的身份。
- 开发机之间保持身份独立：不要在家目录之间复制 SSH 主机密钥、machine-id、遥测 ID、`~/.claude` 或 `~/.claude.json`。工具凭据和 SSH 客户端密钥只按 `internal/seed/seed.go` 里的 `Credentials` 共享，往里加东西前先征得使用者同意。新家目录只用 `cc-runtime seed-home` 初始化。
- 代理配置：先改 `proxy/config.example.yaml`，再把同样的改动应用到每份实际配置（`proxy/config-pennsylvania.yaml`、`proxy/config-california.yaml`），然后运行 `cc-runtime check proxy`。模板里不能出现真实凭据或节点/VPS 地址，见 `proxy/README.md`。

## 工作区

- 开发机把 Mac 工作区挂在和 Mac 相同的路径上，容器里的 Read/Edit 和 Mac 上的 Bash 看到同一个文件。不要改成别的挂载路径。
- 不改动 Mac 工作区里的项目内容；开发机的文件（例如入口脚本生成的说明）写在家目录里。

## 代码与文档

- Go 代码，依赖只有 `golang.org/x/sys`、`golang.org/x/term` 和 `go-runewidth`，加依赖前先征得使用者同意。用户可见的输出和错误用中文；给使用者的错误用 `util.Errorf`。改动要有测试；需要 Docker 的测试用 `CC_RUNTIME_DOCKER_TEST=1` 显式开启。平台相关的代码按 `_darwin.go`、`_linux.go` 分文件；remote-shell 的客户端跑在 Linux、服务跑在 macOS，改完用 `go vet ./...` 和 `GOOS=darwin go vet ./...` 确认两边都能编译。
- `docs/` 只写系统现在的设计和取舍。落地过程、验证记录、本机运维细节放在 `docs/local/`（不入库）。
- 文档和提交信息用中文；提交类型用英文（`feat`、`fix`、`docs`、`refactor` 等）。
