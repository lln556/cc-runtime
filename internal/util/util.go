// Package util 是各命令共用的小工具：查找和运行外部命令、遍历目录、格式化大小。
package util

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// UserError 是给使用者看的错误：只打印消息，不附带内部细节。
type UserError struct{ Msg string }

func (e *UserError) Error() string { return e.Msg }

func Errorf(format string, args ...any) error {
	return &UserError{Msg: fmt.Sprintf(format, args...)}
}

func searchPath() []string {
	home, _ := os.UserHomeDir()
	return []string{"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin",
		filepath.Join(home, ".local/bin"), filepath.Join(home, ".cargo/bin")}
}

// Which 在 PATH 和几个常见目录里查找命令；launchd 下的 PATH 很短。
func Which(name string) string {
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	for _, dir := range searchPath() {
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	return ""
}

func Require(name string) (string, error) {
	if path := Which(name); path != "" {
		return path, nil
	}
	return "", Errorf("%s 未安装或不在 PATH 中", name)
}

type Opts struct {
	Capture   bool     // 收集输出，不直接打印
	AllowFail bool     // 非零退出码不算错误
	Env       []string // 追加的环境变量
	Stdin     string
}

type Result struct {
	Stdout, Stderr string
	Code           int
}

// CommandFailed 是外部命令以非零退出码结束。
type CommandFailed struct {
	Args   []string
	Code   int
	Stderr string
}

func (e *CommandFailed) Error() string {
	msg := fmt.Sprintf("命令失败（退出码 %d）：%s", e.Code, strings.Join(e.Args, " "))
	if detail := strings.TrimSpace(e.Stderr); detail != "" {
		msg += "\n" + detail
	}
	return msg
}

func Run(args []string, opts Opts) (Result, error) {
	cmd := exec.Command(args[0], args[1:]...)
	if opts.Env != nil {
		cmd.Env = append(os.Environ(), opts.Env...)
	}
	if opts.Stdin != "" {
		cmd.Stdin = strings.NewReader(opts.Stdin)
	}
	var stdout, stderr bytes.Buffer
	if opts.Capture {
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
	} else {
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	}
	err := cmd.Run()
	result := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.Code = exitErr.ExitCode()
		if opts.AllowFail {
			return result, nil
		}
		return result, &CommandFailed{Args: args, Code: result.Code, Stderr: result.Stderr}
	}
	if err != nil {
		return result, err
	}
	return result, nil
}

// Lexists 判断路径是否存在（不跟随符号链接）。
func Lexists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// Resolve 转成绝对路径，并解析其中已存在部分的符号链接（不存在的部分保持原样）。
// macOS 上 /var、/tmp 都是链接，比较路径前必须这样处理。
func Resolve(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	rest := ""
	for dir := abs; ; dir = filepath.Dir(dir) {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(real, rest)
		}
		if filepath.Dir(dir) == dir {
			return abs
		}
		rest = filepath.Join(filepath.Base(dir), rest)
	}
}

func IsDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func HumanBytes(value float64) string {
	for _, unit := range []string{"B", "KB", "MB", "GB", "TB"} {
		if value < 1024 && value > -1024 || unit == "TB" {
			if unit == "B" {
				return fmt.Sprintf("%.0f %s", value, unit)
			}
			return fmt.Sprintf("%.1f %s", value, unit)
		}
		value /= 1024
	}
	return ""
}
