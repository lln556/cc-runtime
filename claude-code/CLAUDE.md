# 执行环境（remote-shell）

Bash 工具里的命令经 remote-shell 在宿主 Mac 上执行，运行在使用者的登录会话里，不在当前 Linux 容器里：
`uname`、Homebrew、Xcode、钥匙串、已安装的依赖都以 Mac 为准；sed、find、date、grep 等是 BSD 版本，写命令时按 macOS 的参数习惯。
系统信息里的 `Platform`、`OS Version` 描述的是容器，不是命令执行的地方。会话开始时注入的上下文里有 Mac 的系统版本、架构、共享目录和临时目录；判断执行环境以它和 `uname` 为准。

Read、Edit、Write、Grep、Glob 在容器里执行，只有共享目录里的文件两侧相同：工作区、下载目录（和 Mac 路径相同），以及 Claude Code 的临时目录（scratchpad）。
容器的家目录 `/home/dev` 和 `/tmp` 在 Mac 上不存在或不是同一个目录，不要在那里放要给命令用的文件。

Claude Code 要在共享目录里启动。在 `/home/dev` 等 Mac 上不存在的目录里启动时，Bash 的每条命令都会被拒绝（命令里的 cd 也没用），这时告诉使用者在共享目录里重新启动 Claude Code。

Mac 不可达时 Bash 会报「无法连接 Mac 上的 remote-shell 服务」，不会回退到容器执行。
确需容器内的真实环境时（容器侧的工具、`/home/dev` 下的文件），在 Bash 里用 `docker exec <容器名> sh -c '…'`，容器名见会话开始时的上下文。
