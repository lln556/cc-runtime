package remoteshell

import (
	"path/filepath"
	"strings"

	"github.com/zeke-chin/cc-runtime/internal/config"
)

// 原生安装的 Claude Code 把 ugrep、bfs、ripgrep 打包在自己的二进制里，在 shell 快照里定义同名函数，
// 接管 Bash 的 grep、find、rg（同时停用了 Grep、Glob 工具）。开发机里的快照在 Mac 上不起作用，
// 服务在每条命令前定义同样的函数，指向 RUNTIME_DIR/search-tools 里的程序。程序从哪来由使用者决定
// （链接到 Mac 上的 Claude Code、Homebrew、自己下载……），cc-runtime check search-tools 只检查。
// 函数体照 Claude Code 2.1.309 的 utils/bash/ShellSnapshot.ts（tot、hEr、pEr）改写。

// SearchToolsDir 是放 ugrep、bfs、rg、ug 的目录。
func SearchToolsDir(cfg config.Config) string {
	return filepath.Join(cfg.Runtime, "search-tools")
}

var (
	vcsDirs     = []string{".git", ".svn", ".hg", ".bzr", ".jj", ".sl"}
	findArgs    = []string{"-S", "dfs", "-regextype", "findutils-default"}
	grepArgs    = append([]string{"-G", "--ignore-files", "--hidden", "-I"}, excludeDirs()...)
	grepRawArgs = []string{"-*-filter*", "-*-pager*", "-*-view*", "-*-format-open*", "-*-config*", "---*", "-@*",
		"-*-save-config*", "-[Zz]*", "-[!-]*[Zz]*", "--null", "--null-data"} // ugrep 不兼容的参数，交给系统的 grep
)

func excludeDirs() []string {
	var args []string
	for _, dir := range vcsDirs {
		args = append(args, "--exclude-dir="+dir)
	}
	return args
}

// shellQuote 用单引号包住 s。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// wrapper 生成一个函数：name 运行 program 并带上固定参数；
// raw 里的参数出现时、或 program 不可执行时，退回系统的同名命令。
func wrapper(name, program string, args, raw []string) string {
	var b strings.Builder
	b.WriteString("function " + name + " {\n")
	if len(raw) > 0 {
		b.WriteString("  local _cc_a\n")
		b.WriteString("  for _cc_a in ${1+\"$@\"}; do\n")
		b.WriteString("    case \"$_cc_a\" in " + strings.Join(raw, "|") + ") command " + name + " ${1+\"$@\"}; return ;; esac\n")
		b.WriteString("  done\n")
	}
	b.WriteString("  if [[ ! -x " + shellQuote(program) + " ]]; then command " + name + " ${1+\"$@\"}; return; fi\n")
	fixed := ""
	if len(args) > 0 {
		fixed = strings.Join(args, " ") + " "
	}
	b.WriteString("  " + shellQuote(program) + " " + fixed + "${1+\"$@\"}\n")
	b.WriteString("}\n")
	return b.String()
}

// SearchFunctions 返回在命令前定义 grep、find、rg 函数的 zsh 代码。
// rg 和原生一样，只在 PATH 里没有 rg、而目录里有时才定义；ug 同理。
func SearchFunctions(dir string) string {
	var b strings.Builder
	// ug 是 ugrep 的简称；原生不接管，这里和 rg 一样在 PATH 里没有时提供。
	for _, name := range []string{"rg", "ug"} {
		program := filepath.Join(dir, name)
		b.WriteString("if ! hash " + name + " 2>/dev/null && ! typeset -f " + name + " >/dev/null 2>&1 && [[ -x " +
			shellQuote(program) + " ]]; then\n")
		b.WriteString(wrapper(name, program, nil, nil))
		b.WriteString("fi\n")
	}
	b.WriteString("unalias find 2>/dev/null || true\n")
	b.WriteString("unalias grep 2>/dev/null || true\n")
	b.WriteString(wrapper("find", filepath.Join(dir, "bfs"), findArgs, nil))
	b.WriteString(wrapper("grep", filepath.Join(dir, "ugrep"), grepArgs, grepRawArgs))
	return b.String()
}
