# 执行环境（remote-shell）

`run_command` 工具里的命令经 remote-shell 在宿主 Mac 上执行，运行在使用者的登录会话里，不在当前 Linux 容器里：
`uname`、Homebrew、Xcode、钥匙串、已安装的依赖都以 Mac 为准。
系统信息里的 `Operating System`、`Shell` 描述的是运行 Antigravity 的容器，不是命令执行的地方。会话开始时注入的上下文里有 Mac 的系统版本、架构、共享目录和临时目录；判断执行环境以它和 `uname` 为准。

`read_file`、`edit_file`、`write_file` 等文件工具在容器里执行。共享目录在两侧是同一份文件、同一个路径：工作区、下载目录，以及 remote-shell 的共享临时目录。在共享目录里：

- `run_command` 的起始目录就是当前工作目录，不需要先 cd 到工作区。
- 文件工具和 `run_command` 看到的是同一个文件，选哪种工具照常规规则来，不需要因为跨机器而回避其中一种。

只有共享目录以外的路径两侧不同：
- 容器的家目录 `/home/dev`、`/tmp` 和 `<appDataDir>/brain/.../scratch` 在 Mac 上不存在或不是同一个目录，不要在那里放要给命令用的脚本或文件；临时文件放在共享工作区或会话上下文给出的共享临时目录。
- Mac 上的其他路径只有 `run_command` 能访问，`read_file` 等工具看不到。

Antigravity 会话要在共享目录里启动。在 `/home/dev` 等 Mac 上不存在的目录里启动时，`run_command` 的每条命令都会被拒绝（命令里的 cd 也没用），这时告诉使用者在共享目录里重新开启会话。

Mac 不可达时 `run_command` 会报「无法连接 Mac 上的 remote-shell 服务」，不会回退到容器执行。remote-shell 不提供交互式终端输入（`send_input`），命令需以非交互方式运行。
确需容器内的真实环境时（容器侧的工具、`/home/dev` 下的文件），在 `run_command` 里用 `docker exec <容器名> sh -c '…'`，容器名见会话开始时的上下文。
