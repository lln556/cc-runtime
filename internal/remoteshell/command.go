package remoteshell

import (
	"fmt"
	"sort"
	"strings"
)

// Invocation 是 Claude Code 对 shell 的一次调用：`<shell> [选项] -c <命令> [参数...]`。
type Invocation struct {
	Command string
	Args    []string
}

// ParseArgs 解析 shell 参数。Claude Code 只用两种形式：执行命令时 `-c <命令>`，
// 创建快照时 `-c -l <脚本>`。其他选项（-l、-i 等）照收照忽略；没有 -c 时报错。
func ParseArgs(args []string) (Invocation, error) {
	hasC := false
	i := 0
	for ; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			i++
			break
		}
		if len(arg) < 2 || (arg[0] != '-' && arg[0] != '+') {
			break
		}
		if arg[0] == '-' && arg[1] != '-' && strings.ContainsRune(arg[1:], 'c') {
			hasC = true
		}
	}
	if !hasC {
		return Invocation{}, fmt.Errorf("remote-shell 只支持 `-c <命令>` 的调用形式")
	}
	if i >= len(args) {
		return Invocation{}, fmt.Errorf("remote-shell：-c 后面缺少命令")
	}
	return Invocation{Command: args[i], Args: args[i+1:]}, nil
}

// shellWord 从 s 开头解析一个 shell 词（支持单引号、双引号、反斜杠转义），
// 返回它的值和占用的长度。遇到未加引号的空白或结尾时结束。
func shellWord(s string) (string, int, bool) {
	var b strings.Builder
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == ';' || c == '&' || c == '|':
			return b.String(), i, i > 0
		case c == '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return "", 0, false
			}
			b.WriteString(s[i+1 : i+1+end])
			i += end + 2
		case c == '"':
			i++
			closed := false
			for i < len(s) {
				if s[i] == '"' {
					closed = true
					i++
					break
				}
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`", s[i+1]) >= 0 {
					b.WriteByte(s[i+1])
					i += 2
					continue
				}
				if s[i] == '$' || s[i] == '`' {
					return "", 0, false // 含展开的路径不处理
				}
				b.WriteByte(s[i])
				i++
			}
			if !closed {
				return "", 0, false
			}
		case c == '\\':
			if i+1 >= len(s) {
				return "", 0, false
			}
			b.WriteByte(s[i+1])
			i += 2
		case c == '$' || c == '`':
			return "", 0, false
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), i, i > 0
}

const cwdMarker = "pwd -P >| "

// CwdFileVar 是服务在命令环境里设置的变量，指向 Mac 上的临时 cwd 文件。
const CwdFileVar = "CC_REMOTE_SHELL_CWD_FILE"

// RewriteCwd 把命令末尾的 `pwd -P >| <文件>` 改成写到 $CC_REMOTE_SHELL_CWD_FILE，
// 返回改写后的命令和原来的文件路径。格式不认识时 ok 为假，命令原样返回。
func RewriteCwd(command string) (rewritten, cwdFile string, ok bool) {
	index := strings.LastIndex(command, cwdMarker)
	if index < 0 {
		return command, "", false
	}
	rest := command[index+len(cwdMarker):]
	path, n, ok := shellWord(rest)
	if !ok || path == "" || strings.TrimSpace(rest[n:]) != "" {
		return command, "", false
	}
	return command[:index] + cwdMarker + `"$` + CwdFileVar + `"` + rest[n:], path, true
}

// SnapshotFile 识别 Claude Code 创建 shell 快照的调用，返回快照文件路径。
func SnapshotFile(command string) (string, bool) {
	if !strings.Contains(command, `"$SNAPSHOT_FILE"`) {
		return "", false
	}
	const key = "SNAPSHOT_FILE="
	for start := 0; ; {
		index := strings.Index(command[start:], key)
		if index < 0 {
			return "", false
		}
		index += start
		start = index + len(key)
		// 必须是一条赋值语句的开头，而不是 "$SNAPSHOT_FILE" 之类的引用。
		if index > 0 && !strings.ContainsRune(" \t\n;&|", rune(command[index-1])) {
			continue
		}
		if path, _, ok := shellWord(command[start:]); ok && path != "" {
			return path, true
		}
	}
}

// SnapshotContent 是在开发机里代写的快照：命令在 Mac 上执行，环境由服务的快照提供。
const SnapshotContent = "# Snapshot file\n# cc-runtime remote-shell：命令在 Mac 上执行，Mac 的环境由 remote-shell 服务提供。\n"

// 带到 Mac 的环境变量白名单。其余（PATH、HOME、TMPDIR、容器里的 socket 等）只属于开发机。
var passEnv = map[string]bool{
	"CLAUDECODE":             true,
	"CLAUDE_CODE_ENTRYPOINT": true,
	"CLAUDE_CODE_SESSION_ID": true,
	"AI_AGENT":               true,
	"CODEX_CI":               true,
	"GIT_EDITOR":             true,
	"EDITOR":                 true,
}

// EnvPrefix 开头的变量去掉前缀后带到 Mac：留给使用者显式传值。
const EnvPrefix = "CC_REMOTE_ENV_"

// FilterEnv 从 KEY=VALUE 列表里挑出要带到 Mac 的变量。
func FilterEnv(environ []string) map[string]string {
	result := map[string]string{}
	var prefixed []string
	for _, item := range environ {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if passEnv[key] {
			result[key] = value
		} else if strings.HasPrefix(key, EnvPrefix) && len(key) > len(EnvPrefix) {
			prefixed = append(prefixed, item)
		}
	}
	// 显式传的值优先于白名单里的同名变量。
	sort.Strings(prefixed)
	for _, item := range prefixed {
		key, value, _ := strings.Cut(item, "=")
		result[strings.TrimPrefix(key, EnvPrefix)] = value
	}
	return result
}
