# remote-shell

remote-shell 让开发机里的 Claude Code 把 Bash 命令交给 Mac 执行：Claude Code 进程、它的身份和 API 出口仍在开发机里，命令在使用者的 Mac 登录会话里运行，看到的是 Mac 的 PATH、Homebrew、Xcode、钥匙串和已装的依赖。项目只需要一套 Mac 依赖，不必再维护 Linux 版本。

在 `.env` 的 `REMOTE_SHELL_MACHINES` 里列出的开发机启用；其余开发机保持原样。

## 组成

```
开发机（Linux，私网）                  网关（Mihomo）                 Mac（GUI 登录会话）
claude                                 DIRECT tunnel                  LaunchAgent io.cc-runtime.remote-shell
 └─ /opt/cc-runtime/remote-shell/zsh   <网关 IP>:10024                cc-runtime remote-shell serve
    -c <命令>          ── TCP，帧协议 ──►  → host.docker.internal:47100 ──► 127.0.0.1:47100
                       ◄─ stdout/stderr/退出码 ─                        └─ 新进程组：/bin/zsh -c <命令>
          │                                                                     │
          └──────── 同路径的共享目录：WORKSPACE_DIR、remote-shell 目录 ─────────┘
```

| 部分 | 位置 | 作用 |
|---|---|---|
| 客户端 | 开发机 `/opt/cc-runtime/remote-shell/zsh`（`RUNTIME_DIR/bin` 只读挂载） | `CLAUDE_CODE_SHELL`；每条命令建立一条连接，转发输出、信号和心跳，写回 cwd |
| 服务 | Mac，LaunchAgent，监听 `127.0.0.1:47100` | 认证、执行、清理进程组；缓存 Mac 登录 shell 的环境 |
| tunnel | 开发机自己的网关，私网地址的 10024 端口 | 只开在私网，不发布到宿主机；不经过代理链 |
| 每机目录 | `RUNTIME_DIR/remote-shell/<机器>/` | `client.json`（地址和 token）、`tmp/`（Claude Code 的临时目录） |

客户端和服务是同一个 `cc-runtime` 程序：经 `…/remote-shell/zsh` 链接运行时是客户端。`scripts/install.sh` 在 Mac 上安装 darwin 版，同时把 linux 版交叉编译到 `RUNTIME_DIR/bin/cc-runtime-linux`，并建立 `remote-shell/zsh` 链接。开发机挂的是这个目录而不是单个文件：重新安装是改名替换，挂单个文件会一直指向旧文件。

## 设计取舍

### 为什么不用 ssh 执行

ssh 进来的进程在 macOS 的 `Background` 会话里，不在使用者的 GUI 登录会话（`Aqua`）里：钥匙串读不出（`User interaction is not allowed`），`gh` 等把凭据放在钥匙串里的工具会失败，而且报错很迷惑（gh 报 token 无效）。LaunchAgent 运行在 `Aqua` 会话里，和终端 App 一样。所以命令由 LaunchAgent 执行，开发机直接连它。

把 Mac 上的 Unix socket 挂进容器在 OrbStack 下连不上（`Connection refused`），所以传输用 TCP，经网关的 DIRECT tunnel 到 `host.docker.internal`。OrbStack 把它转到 Mac 的回环地址，服务只需绑定 `127.0.0.1`。

### 每条命令一个进程、一条连接

Claude Code 本来就是每条命令启动一个新 shell，命令之间只延续 cwd 和启动时的快照。remote-shell 保持同样的语义：每条命令一条 TCP 连接，Mac 上一个新的 `/bin/zsh -c`，连接的生命周期就是命令的生命周期。常驻一个远端 shell 会让并行命令和后台任务互相阻塞，前一条命令的 `export`、`set -e` 还会影响后一条。连接经私网和宿主转发建立，开销在毫秒级，不需要复用。

### 不分配 pty

pty 会把 stdout 和 stderr 合成一路，输出带 `\r\n`，程序还会因为看到 tty 输出颜色、进度条或启动分页器。命令的 stdin 是 `/dev/null`，stdout、stderr 各一条管道，和 Claude Code 本地执行时一样。

### zsh

Mac 的登录 shell 是 zsh，系统自带的 bash 是 3.2。客户端以 zsh 的身份出现，Claude Code 就生成 zsh 的命令前缀，Mac 上用 `/bin/zsh` 执行，和在 Mac 上直接运行 Claude Code 一致。Claude Code 只接受路径里含 `bash` 或 `zsh` 的 `CLAUDE_CODE_SHELL`，所以名字放在目录上（`remote-shell/zsh`）。

### 工作区直接挂载

启用的开发机直接挂 Mac 的 `WORKSPACE_DIR`（同一路径），不再挂 Linux 副本：Edit/Write 在容器里写入，紧接着的 Bash 在 Mac 上就能读到；依赖只有 Mac 一套；git 只在 Mac 一侧写。挂载由 `cc-runtime up` 生成的 Compose 叠加配置完成（Compose 按挂载目标合并，替换 compose.yaml 里的 Linux 副本）。

### 连不上时报错，不回退

服务不可达时客户端以 255 退出，报「无法连接 Mac 上的 remote-shell 服务」，不会改在开发机里执行。同一条命令在不同时刻跑在不同系统上，比失败更难排查。临时回到 Linux 执行可以设置 `REMOTE_SHELL_LOCAL=1` 后重启 Claude Code。

## 和 Claude Code 的衔接

Claude Code 对 `CLAUDE_CODE_SHELL` 的调用形如：

```
zsh -c 'source <快照> 2>/dev/null || true && setopt … && eval '\''<命令>'\'' < /dev/null && pwd -P >| <cwd 文件>'
```

cwd 由子进程的工作目录传递，环境变量由进程环境传递。客户端这样处理：

| 项目 | 处理 |
|---|---|
| 工作目录 | 带上客户端的 cwd；Mac 上不存在时报错（Bash 的工作目录必须在共享路径里） |
| cwd 回写 | 把末尾的 `pwd -P >| <文件>` 改成写到服务提供的临时文件，服务在退出帧里带回内容，客户端写到原来的文件。不依赖共享文件系统的缓存一致性 |
| 快照 | Claude Code 启动时用 `-c -l <脚本>` 创建 shell 快照。客户端识别这类调用，在开发机里直接写一个空快照；命令前缀里的 `source` 在 Mac 上找不到文件，是空操作。Mac 的环境由服务提供 |
| 环境变量 | 只带 `CLAUDECODE`、`CLAUDE_CODE_ENTRYPOINT`、`CLAUDE_CODE_SESSION_ID`、`AI_AGENT`、`GIT_EDITOR`、`EDITOR`；`CC_REMOTE_ENV_<名字>` 去掉前缀后带过去。PATH、HOME、TMPDIR、容器里的 socket 一律不带 |
| 临时目录 | `CLAUDE_CODE_TMPDIR` 设为每机目录里的 `tmp/`（同路径挂载）：Write 写进 scratchpad 的脚本，Mac 上的命令也能运行 |

开发机的 `~/.claude/settings.json` 要有：

```json
{
  "env": {
    "CLAUDE_CODE_SHELL": "/opt/cc-runtime/remote-shell/zsh",
    "CLAUDE_CODE_TMPDIR": "<RUNTIME_DIR>/remote-shell/<机器>/tmp"
  }
}
```

系统提示里的 `Platform` 仍是 linux，所以 `~/.claude/CLAUDE.md` 要说明 Bash 在 Mac 上执行、哪些路径两侧共享（`cc-runtime check remote-shell` 检查这两处）。家目录不由 cc-runtime 改写，这两个文件由使用者维护。

## 协议

一条 TCP 连接承载一条命令。帧格式：类型 1 字节、长度 4 字节（大端）、内容；单帧上限 1 MiB，输出按 64 KiB 切块。

| 类型 | 方向 | 内容 |
|---|---|---|
| `Q` 请求 | 开发机 → Mac | JSON：协议版本、token、cwd、命令、参数、环境变量；必须是第一帧，5 秒内到达 |
| `S` 信号 | 开发机 → Mac | `TERM`、`INT` 或 `HUP`，转发给整个进程组 |
| `P` 心跳 | 双向 | 每 15 秒一次 |
| `O`/`E` 输出 | Mac → 开发机 | stdout、stderr 字节 |
| `X` 退出 | Mac → 开发机 | 退出码（被信号杀死时 128+信号值）、cwd；最后一帧 |
| `F` 失败 | Mac → 开发机 | 退出码和消息（认证失败、cwd 不存在、版本不一致、服务重启）；最后一帧 |

命令和参数可能含非 UTF-8 字节，在 JSON 里以 base64 原样往返。不做应用层流控：开发机不读，Mac 端写帧就阻塞，命令写 stdout 随之阻塞，和本地管道一样。

### 结束与清理

| 情况 | 处理 |
|---|---|
| 命令正常结束 | 等输出管道关闭（最多 1 秒，后台进程可能一直持有 stdout），发退出帧。命令留下的后台进程继续运行，和本地 shell 一样 |
| 客户端收到 TERM/INT/HUP（超时、Esc） | 转发信号帧，等 3 秒退出帧；没等到就关闭连接 |
| 连接断开（客户端被杀，包括 SIGKILL） | 服务给进程组发 SIGTERM，3 秒后仍在就发 SIGKILL |
| 45 秒收不到对方的任何帧 | 双方都认为连接已死：服务清理进程组，客户端以 255 退出。中间任何一层的空闲超时或半开连接都靠它发现 |
| 服务停止或重启 | 给所有进行中的命令发失败帧，清理它们的进程组 |

命令自己用 `setsid` 脱离进程组的进程不会被清理，和本地一样。

### Mac 的环境

launchd 启动的进程只有系统目录的 PATH，而每条命令都启动登录 shell 要 0.5 秒，还会混入 `.zshrc` 插件的输出。服务运行一次 `zsh -l -i -c`，取出前后标记之间 NUL 分隔的 `env -0` 输出作为快照，缓存在内存和 `~/Library/Caches/cc-runtime/remote-shell/env.json`。`~/.zshenv`、`.zprofile`、`.zshrc`、`.zlogin` 的修改时间变化或超过 24 小时后重新生成。快照只有环境变量，不含 alias 和函数（例如 `nvm use` 这类 shell 函数不可用）。命令用非登录的 `zsh -c` 执行，只读 `~/.zshenv`。

## 部署

1. `.env` 里设置 `REMOTE_SHELL_MACHINES`（逗号分隔），端口默认 47100（`REMOTE_SHELL_PORT`，改了要同时改网关配置里的 tunnel）。
2. 网关配置的 `GATEWAY-SPECIFIC: tunnels` 块里有：
   ```yaml
   - network: [tcp]
     address: <网关 IP>:10024
     target: host.docker.internal:47100
     proxy: DIRECT
   ```
   网关的入口脚本把私网上的 10023、10024 端口排除在透明代理之外，并只对自己的开发机放行。
3. 在 Mac 上：`scripts/install.sh`，然后 `cc-runtime remote-shell install`（写入 LaunchAgent 并启动）。
4. `cc-runtime up <网关> <开发机> --force-recreate`：生成每机目录和 token，挂载 Mac 工作区、每机目录（同路径，以及只读挂到 `/var/lib/cc-runtime/remote-shell`）和 `/opt/cc-runtime`。
5. 配置开发机的 `~/.claude/settings.json` 和 `CLAUDE.md`（见上），运行 `cc-runtime check remote-shell`。它检查服务和版本、tunnel、挂载、Claude Code 配置，并在开发机里端到端执行 `uname -s; launchctl managername`，应得到 `Darwin Aqua`。

LaunchAgent 的 `ProcessType` 是 `Interactive`：默认的后台类型会被降低 CPU/IO 优先级。服务日志在 `RUNTIME_DIR/logs/remote-shell.log`（连接、拒绝、每条命令的退出码和用时，命令截断到 160 字符；不记录输出）；进行中的命令写在 `RUNTIME_DIR/state/remote-shell.json`。

重新安装后 `install.sh` 会重启服务；有命令正在运行时只提示，不中止它们，确认后运行 `cc-runtime remote-shell restart --force`。

## 边界

- **认证**：服务监听 Mac 的回环地址，但 OrbStack 下任何容器都能经 `host.docker.internal` 连到它，所以每个请求都要带 token。每台开发机一个 32 字节随机 token，在 `client.json`（0600）里；服务每个请求都重新读取 `.env` 的机器列表和 token，停用即时生效。开发机里能读 token 的进程本来就能通过 remote-shell 执行命令，不扩大权限。流量不出这台 Mac，不加密。
- **执行隔离**：命令以使用者的 Mac 账户在登录会话里运行，能访问该账户能访问的一切，包括钥匙串。开发机不再是命令的沙箱。
- **出口**：命令的网络流量走 Mac 自己的网络，不经过开发机的网关；「没有直连回退」只对 Claude Code 自己的流量成立。
- **共享工作区**：直接挂载后，开发机能看到工作区里的一切，包括本仓库的 `dev-runtimes/`（其他开发机的家目录）、`.env` 和网关配置。
- **文件访问授权**：命令访问 Downloads、Documents、Desktop 等受保护目录时，macOS 把授权记在服务的二进制上，第一次会在 Mac 上弹框，命令等待期间挂起。未签名的二进制重新编译后签名会变，授权可能要重给；设置 `CC_RUNTIME_CODESIGN_IDENTITY` 后 `install.sh` 用固定的证书签名。工作区不受影响。
- **路径**：Bash 的工作目录和命令用到的文件必须在共享路径里；开发机的 `/home/dev`、`/tmp` 在 Mac 上不是同一个目录。
