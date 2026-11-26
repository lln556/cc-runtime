package remoteshell

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Codex CLI 的用户 shell 由 getpwuid 从 /etc/passwd 取（入口脚本把 dev 的登录 shell 指向
// CodexShellPath），它自己不带 CLAUDE_CODE_SHELL 那样的配置项。命令环境里的
// CC_CODEX_REMOTE_SHELL=1 来自 /etc/codex/config.toml 的 shell_environment_policy，
// 所以「是不是 Codex 发出的命令」由这个变量判断，而不是靠 shell 名字。
const (
	CodexDirName          = "codex-shell"
	CodexShellPath        = BinDir + "/" + CodexDirName + "/zsh"
	CodexSessionStartPath = BinDir + "/" + CodexDirName + "/" + SessionStartName
	CodexRemoteEnv        = "CC_CODEX_REMOTE_SHELL"
	testLocalZshEnv       = "CC_TEST_LOCAL_ZSH"
)

type codexRoute int

const (
	codexRouteLocal codexRoute = iota
	codexRouteRemote
)

func classifyCodexShell(args []string, env map[string]string) codexRoute {
	if env[CodexRemoteEnv] != "1" || env[LocalSwitch] == "1" {
		return codexRouteLocal
	}
	if _, err := ParseArgs(args); err != nil {
		return codexRouteLocal
	}
	return codexRouteRemote
}

func resolveLocalZsh() string {
	if path := os.Getenv(testLocalZshEnv); path != "" {
		return path
	}
	for _, candidate := range []string{"/bin/zsh", "/usr/bin/zsh"} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return "/bin/sh"
}

// prepareLocalZshEnv 去掉 CC_CODEX_REMOTE_SHELL，避免本地脚本再触发一次远端路由；
// 同时把 SHELL 从 CodexShellPath 改回本地 zsh，SSH 会话、Herdr 和子 shell 看到的才是
// 一个普通的登录 shell。
func prepareLocalZshEnv(environ []string, localZsh string) []string {
	result := make([]string, 0, len(environ))
	for _, item := range environ {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			result = append(result, item)
			continue
		}
		if key == CodexRemoteEnv {
			continue
		}
		if key == "SHELL" && value == CodexShellPath {
			result = append(result, "SHELL="+localZsh)
			continue
		}
		result = append(result, item)
	}
	return result
}

// CodexShellMain 是 codex-shell/zsh 的入口，参数和 shell 相同，返回退出码。
// fullArgs 含 argv[0]：sshd 用 "-zsh" 调用登录 shell，必须原样保留。
func CodexShellMain(fullArgs []string) int {
	env := map[string]string{}
	for _, item := range os.Environ() {
		if key, value, ok := strings.Cut(item, "="); ok {
			env[key] = value
		}
	}
	var args []string
	if len(fullArgs) > 0 {
		args = fullArgs[1:]
	}
	if classifyCodexShell(args, env) == codexRouteLocal {
		zsh := resolveLocalZsh()
		execArgs := fullArgs
		if len(execArgs) == 0 {
			execArgs = []string{filepath.Base(zsh)}
		}
		if err := syscall.Exec(zsh, execArgs, prepareLocalZshEnv(os.Environ(), zsh)); err != nil {
			errorf(os.Stderr, "无法在本地执行 %s：%v", zsh, err)
			return 127
		}
		return 0
	}
	// Codex 和 Antigravity 一样可能从 ~/workspace、~/personal 这类符号链接进入；
	// 容器里的 $PWD 是链接路径，Mac 上不存在，先解析成物理路径。
	if cwd, err := os.Getwd(); err == nil {
		if real, err := filepath.EvalSymlinks(cwd); err == nil && real != "" && real != cwd {
			if os.Chdir(real) == nil {
				os.Setenv("PWD", real)
			}
		}
	}
	return ClientMain(args)
}

// CodexSessionStartMain 是 Codex 的 SessionStart hook：探测结果按 Codex 的
// hookSpecificOutput 格式输出，并先把未做 realpath 的符号链接 cwd 解析为物理路径。总是返回 0。
func CodexSessionStartMain(stdin io.Reader, stdout io.Writer) int {
	return runSessionStart(stdin, stdout, codexAgent, true)
}
