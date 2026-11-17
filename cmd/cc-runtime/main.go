// cc-runtime 是开发运行环境的统一入口。用 scripts/install.sh 安装到 ~/.local/bin。
//
// 经 …/remote-shell/zsh 链接运行时，它是开发机里的 remote-shell 客户端（CLAUDE_CODE_SHELL）。
package main

import (
	"os"
	"path/filepath"

	"github.com/zeke-chin/cc-runtime/internal/cli"
	"github.com/zeke-chin/cc-runtime/internal/remoteshell"
)

func main() {
	if filepath.Base(filepath.Dir(os.Args[0])) == "remote-shell" {
		os.Exit(remoteshell.ClientMain(os.Args[1:]))
	}
	os.Exit(cli.Main(os.Args[1:]))
}
