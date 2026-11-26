// cc-runtime 是开发运行环境的统一入口。用 scripts/install.sh 安装到 ~/.local/bin。
//
// 按入口文件名分派：…/remote-shell/zsh 是 Claude Code 的 remote-shell 客户端，
// …/remote-shell/session-start 是它的 SessionStart hook；…/codex-shell/zsh 是 Codex 的
// 用户 shell（按命令环境决定留在容器还是交给 Mac），…/codex-shell/session-start 是
// Codex 的 SessionStart hook。其余情况是 CLI。
package main

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/zeke-chin/cc-runtime/internal/cli"
	"github.com/zeke-chin/cc-runtime/internal/remoteshell"
)

func main() {
	// sshd 用 "-zsh" 调用登录 shell，前导 "-" 不属于名字。
	dir := filepath.Base(filepath.Dir(os.Args[0]))
	base := strings.TrimPrefix(filepath.Base(os.Args[0]), "-")
	switch {
	case dir == "remote-shell":
		if base == remoteshell.SessionStartName {
			os.Exit(remoteshell.SessionStartMain(os.Stdin, os.Stdout))
		}
		os.Exit(remoteshell.ClientMain(os.Args[1:]))
	case dir == remoteshell.CodexDirName || base == "zsh":
		if base == remoteshell.SessionStartName {
			os.Exit(remoteshell.CodexSessionStartMain(os.Stdin, os.Stdout))
		}
		os.Exit(remoteshell.CodexShellMain(os.Args))
	default:
		os.Exit(cli.Main(os.Args[1:]))
	}
}
