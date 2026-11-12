# 容器 → Mac Codex 桥接

容器客户端把任务写入 bind mount 信箱。Mac 上的串行 worker 通过本机 Unix WebSocket 调用已有 Codex App Server，回传事件、结果和产物。容器不连接宿主机网络；不改 Mihomo、DNS、Tailscale、SSH 主机密钥和依赖总卷。

## 配置与准备

宿主机需要 Python 3.10+、Git，以及有本机控制 socket 的运行中 Codex App Server。这里使用生成配置中指定的 socket，不重启或改写 Codex 服务。

在仓库根目录执行：

```bash
python3 host-agent/setup.py
host-agent/bin/host-agent probe
```

setup 只生成本地配置、状态目录和 LaunchAgent 草稿，不安装服务、不重建容器。`config/local.json` 中的路径相对于该配置文件；`projects` 是允许使用的项目别名及 Mac 实际目录。默认只有 `cc-runtime`。添加 `ios-app` 等项目时填写其真实路径；容器提交完整 HEAD，worker 验证一致后才执行。工作区未提交改动仍共享，双方须暂停冲突写入。

每个项目可配置 `client_paths`，列出调用方所见的绝对 Git 仓库根路径，例如 Mac 的 `/Users/axis/projects/yue` 对应容器的 `["/home/dev/personal/yue"]`。`capabilities.project_paths` 发布各项目的 Mac 路径和调用方路径。客户端从当前工作目录取得 Git 根目录，精确匹配唯一项目；仓库子目录自动归属于该仓库，嵌套的独立仓库须单独配置。没有映射、映射歧义或显式 `--project` 与自动结果不符时，提交失败，不回退到其他项目。服务端再次核对请求中的 `caller_repo`；升级前已排队且不含该字段的任务仍按原有流程处理。修改项目映射后需在空闲时重启 worker，重新发布 capabilities；不要中断运行中的任务。

`probe` 只验证接口与工具发现。`computer_use=advertised` 不代表具体应用、浏览器、手机、macOS 权限已经可用。独立的新任务必须实际调用对应工具才能确认。

可在 `config/local.json` 中指定 `model` 和 `effort`（例如 `"model": "可用模型ID", "effort": "high"`）；留空或设为 null 时采用服务默认值。模型在新建和续接会话时传入，推理强度在每次启动任务时传入。仅宿主机管理配置，不向调用方暴露模型选项。修改后重启接收进程即可生效，无需重建容器；模型及推理强度必须由当前 Provider 支持。

示例配置使用 `"approval_policy": "never"`，项目使用 `"sandbox": "danger-full-access"`，等效于 CLI 的 `--yolo`：命令无需执行审批，也不套 Codex 文件系统沙箱。两项设置同时作用于新建和续接任务。已有配置省略这些字段时仍保留 `on-request` / `workspace-write`；也可以显式设置这两个值以启用执行审批和沙箱。运行中的旧轮次保留原权限，配置变更在新轮次生效。

`herdr.enabled=true` 时，新任务会在 Mac 的 Herdr 中打开同一个 Codex 会话。`herdr.binary` 指定本机 CLI（LaunchAgent 可使用绝对路径），`herdr.session` 默认是 `default`。按项目实际目录匹配 workspace，重名时优先使用与项目别名同名的 workspace；没有匹配项就创建对应 workspace。每个独立会话创建一个以任务短名命名的 tab/pane；由调用方 Agent 理解任务目标后，通过必填的 `--title` 提供 1–5 字的完整任务短名，例如“订阅验收”“后端部署”，不截取任务正文。缺少短名的提交会报错，提示调用方先概括；历史记录缺少标题时仅显示“未命名任务”。续接任务复用仍连接同一会话的 pane，并更新为本轮任务短名，不改变当前焦点。

该 pane 通过 `codex --remote … resume THREAD_ID --cd 项目目录` 连接同一个 App Server，只展示和接入现有会话，不重发任务提示词，模型和权限沿用该会话。任务完成后保留 pane 和输出。`status.view` 提供 workspace/tab/pane ID；Herdr 不可用或打开失败时，`status.view_error` 记录原因，原任务仍继续执行。视图索引位于私有的 `state/host/herdr/`，不挂载给客户端。新面板的 shell 尚未就绪、Herdr 返回 `agent_pane_busy` 时，在同一 pane 内限时重试；其他启动错误仅等待并核对目标 Codex 进程，不重发启动命令。等待窗口为 60 秒，覆盖 Conda 等交互式 shell 初始化耗时；结果仍不明时保留 pane 供检查，不盲目重复启动。

查看面板单独覆盖 `tui.terminal_title=["thread-name","project-name"]`，移除 remote viewer 在轮次结束后可能残留的 `activity` 转圈标题，避免 Herdr 的高优先级标题规则把已完成会话误判为 working。运行和等待输入状态仍由 Herdr 的终端内容规则识别；不接管 pane 生命周期，也不修改用户全局 Codex 配置。修改启动参数后须重启 worker；已打开的查看面板须退出后重新接入才能采用新配置，续接任务仍会复用未退出的旧面板。

## 启用（会断开当前容器会话）

确认安装后，在 Mac 的仓库根目录执行：

```bash
touch host-agent/config/enabled
python3 scripts/dev.py compose config --quiet
python3 scripts/dev.py compose up -d --no-deps --force-recreate dev
cp host-agent/state/host/local.cc-runtime.host-agent.plist "$HOME/Library/LaunchAgents/local.cc-runtime.host-agent.plist"
launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/local.cc-runtime.host-agent.plist"
```

复制前确认目标 LaunchAgent 不存在，不覆盖已有配置。若已加载同名服务，先检查状态，避免重复 bootstrap。只重建 `dev`；现有 SSH/VNC 会话和容器 Agent 进程会中断，持久化家目录、源码、密钥和依赖卷保留。`scripts/dev.py` 会在后续启动时持续带上启用的 overlay。

新增四个挂载：信箱读写、Python 源码只读、CLI 只读、Claude Skill 只读。宿主机配置、审批决定和 worker 日志不挂到容器；若将 `cc-runtime` 放在共享工作区内，容器仍能通过工作区挂载看到仓库目录，需隔离宿主机私有状态与审批目录时应将 `cc-runtime` 置于共享工作区之外。若容器已有同名 Skill 或 CLI，先检查再启用，避免挂载遮住已有内容。运行时文件被 Git 忽略，整个 host-agent 目录不进入 Docker 构建上下文。

## 使用

开发与统筹默认由 Claude Code 承担，Mac 承接需要其工具链、设备、会话或专有访问条件的执行与现场诊断。仅宿主机可达的内网或远端服务器查询、验证及已授权部署属于后一类。完整的委派边界见[调用方 Skill](skills/host-agent/SKILL.md)，执行及交回规则见[Mac 执行约定](execution.md)。

调用方的 Skill 仅介绍 Mac 任务服务及其操作流程，不声明调用方身处容器，不指定其 Agent 身份，也不注入宿主机关系说明。此部署文档保留维护所需的架构信息；不另向 Claude Code 的常驻指令添加环境身份提示。这是接口说明的抽象，不阻止通过系统检查自行判断运行环境。

Skill 目录已只读挂载，编辑后容器可直接读取新内容；已加载旧内容的会话应在下一次分配任务前重新读取 Skill。worker 每次启动任务时读取 `execution.md`（若存在不提交 Git 的 `config/execution.local.md`，自动追加在末尾以补充本机专有范围）并传入新建或续接会话，正在执行的轮次保留原指令。这两份文档的修改无需重建容器或重启 worker。

容器中的 `host-agent capabilities` 显示心跳和项目名单。所有客户端命令输出 JSON；错误退出码为 1。

```bash
host-agent submit --title "构建验收" --commit "$(git rev-parse HEAD)" \
  --task '完成 iOS 构建和 AppLayoutTests，提供日志和失败原因'
host-agent status TASK_ID
host-agent result TASK_ID
host-agent message TASK_ID '补充说明……'
host-agent cancel TASK_ID
host-agent submit --title "失败排查" --after TASK_ID --task '继续检查上一步失败原因'
```

默认任务授权范围仅为本地构建、测试和诊断。`--authorization` 传递用户已经给出的授权，不是新的授权来源；宿主机仍读取项目 AGENTS.md。执行权限由 Mac 配置决定，full-access 不扩大任务中允许操作的范围。需要只读任务时，将项目 sandbox 配为 read-only。

使用 `on-request` 暂停审批，或任务需要额外用户输入时，status 带 pending.method、params、token。人类在 Mac 上核对实际请求后答复，普通命令和文件写入审批：

```bash
host-agent/bin/host-agent respond TASK_ID --token TOKEN --decision accept
# 也可以 decline 或 cancel；只批准这一次，不持久放宽规则。
```

问题回答或其他审批使用 `--result-file` 传入该请求对应的 JSON-RPC result 对象；例如 `item/tool/requestUserInput` 使用 `{"answers":{"问题ID":{"answers":["回答"]}}}`。接口结构以当前 Codex 协议为准。结果不合规会由服务端拒绝；不自动批准、不将凭据请求交给容器。不要让容器通过其他方式写 host_state。

`--mailbox PATH` / `HOST_AGENT_MAILBOX` 指定客户端信箱，前者优先。安装在 `/opt/host-agent` 的客户端默认使用 `/run/host-agent`，SSH 会话无需继承环境变量；源码目录运行时默认使用 `host-agent/state/mailbox`。宿主机管理命令使用 `--config PATH`。容器只安装客户端入口，不能访问 host_state 或宿主机 socket。

## 运行与恢复

- worker 串行执行并用宿主机锁阻止重复启动；不同终端或桌面任务不受该锁控制。
- 任务 ID 写入宿主机日志后，不因重复投递再次执行。请求、状态和结果以临时文件加重命名发布；事件流按行追加。
- 信箱根目录及其内部目录不得使用符号链接。worker 和客户端通过目录描述符读写、发布和清理信箱文件，拒绝链接导致的路径跳转；JSON 读取和事件追加还拒绝硬链接及非普通文件。事件日志在整轮任务中复用已打开的文件描述符。发现不安全路径时拒绝处理或停止 worker，不沿该路径继续写入；排查后保留私有任务日志并按恢复流程重启。
- 超时默认一小时，含等待审批；可以在配置中调整。取消和停止服务会请求中断，并等待终态。中断不回滚已完成的操作。
- 断线或进程崩溃后，先核对并停止旧会话，不自动重放任务。无法确认执行停止时标为 unknown，阻止领取新任务。恢复 Codex 后重启 worker 再核对；不要删除日志来“重试”。
- 服务退出期间仍可排队，但状态会显示离线。capabilities 的工具发现可能滞后；更新 Codex 或插件后重启 worker 重新探测。
- Mac 登录时若 Codex 尚未启动，空闲 worker 每 30 秒重试连接；无需重装服务。
- 这是受信任开发环境中的委派工具，授权文字与共享文件不构成对恶意容器的安全隔离。不要把任意不受信任工作负载接入此信箱。

信箱路径保护要求其宿主机父目录由宿主机管理，且私有状态没有经其他挂载暴露给客户端；它保护桥接自身的文件操作，不替代 Codex 的执行沙箱，也不限制任务执行器通过普通路径写入产物。

查看服务：`launchctl print gui/$(id -u)/local.cc-runtime.host-agent`。日志位于 `state/host/`。停用时 bootout 对应 LaunchAgent，移除 `config/enabled`，再通过 `scripts/dev.py compose` 重建 dev；保留 state 以便检查执行记录。

## 验证

```bash
python3 -m unittest discover -s tests -p 'test_host_agent*.py' -v
python3 -m unittest discover -s tests -p test_dependencies.py -v
```

App Server 参考：`https://developers.openai.com/codex/app-server/`。本机 `codex app-server generate-json-schema --experimental --out DIR` 可导出与已安装版本一致的协议。

2026-11-10 本机验收：完全禁网的临时 Docker 容器通过信箱提交任务，Mac Codex 执行 `uname -s` 返回 Darwin，调用 `cua.getState()` 枚举原生桌面并完成工具摘要，结果由容器成功读回。续接任务复用同一会话并保留上下文；任务标识也写入 Codex 历史，供断线恢复准确定位。浏览器接入在这次验收中报告不可用，未声称通过；尚未测试实际应用点击、真机装包或 iOS 构建。
