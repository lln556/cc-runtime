package remoteshell

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	snapshotBegin = "__CC_REMOTE_SHELL_ENV_BEGIN__"
	snapshotEnd   = "__CC_REMOTE_SHELL_ENV_END__"
)

// SnapshotMaxAge 之后快照会重新生成。
var SnapshotMaxAge = 24 * time.Hour

// 只属于生成快照的那个 shell 进程的变量，不进快照。
var snapshotExclude = map[string]bool{
	"PWD": true, "OLDPWD": true, "SHLVL": true, "_": true, "TERM": true, "TERM_PROGRAM": true,
	"TERM_PROGRAM_VERSION": true, "TERM_SESSION_ID": true, "COLUMNS": true, "LINES": true,
	"ZSH_EXECUTION_STRING": true, "P9K_TTY": true, "P9K_SSH": true, "_P9K_TTY": true, "_P9K_SSH_TTY": true,
}

// EnvSnapshot 缓存 Mac 登录 shell 的环境变量。launchd 启动的进程只有系统目录的 PATH，
// 而每条命令都启动登录 shell 太慢（约 0.5 秒）且会混入 .zshrc 插件的输出。
type EnvSnapshot struct {
	Shell     string        // 生成快照用的 shell，例如 /bin/zsh
	CachePath string        // 持久化位置；空则只缓存在内存里
	Home      string        // 查找 shell 配置文件的家目录
	Timeout   time.Duration // 生成快照的超时
	Logf      func(format string, args ...any)

	mu      sync.Mutex
	env     []string
	sources map[string]int64 // 配置文件 → 修改时间（UnixNano），不存在为 0
	taken   time.Time
}

type snapshotCache struct {
	Env     []string         `json:"env"`
	Sources map[string]int64 `json:"sources"`
	Taken   time.Time        `json:"taken"`
}

func (e *EnvSnapshot) logf(format string, args ...any) {
	if e.Logf != nil {
		e.Logf(format, args...)
	}
}

func (e *EnvSnapshot) configFiles() []string {
	var files []string
	for _, name := range []string{".zshenv", ".zprofile", ".zshrc", ".zlogin"} {
		files = append(files, filepath.Join(e.Home, name))
	}
	return files
}

func (e *EnvSnapshot) currentSources() map[string]int64 {
	result := map[string]int64{}
	for _, path := range e.configFiles() {
		if info, err := os.Stat(path); err == nil {
			result[path] = info.ModTime().UnixNano()
		} else {
			result[path] = 0
		}
	}
	return result
}

func sameSources(a, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// Get 返回 KEY=VALUE 形式的环境；配置文件改过或快照过期时重新生成。生成失败时退回进程自己的环境。
func (e *EnvSnapshot) Get() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	sources := e.currentSources()
	if e.env == nil && e.CachePath != "" {
		if data, err := os.ReadFile(e.CachePath); err == nil {
			var cache snapshotCache
			if json.Unmarshal(data, &cache) == nil && len(cache.Env) > 0 {
				e.env, e.sources, e.taken = cache.Env, cache.Sources, cache.Taken
			}
		}
	}
	if e.env != nil && sameSources(sources, e.sources) && time.Since(e.taken) < SnapshotMaxAge {
		return e.env
	}
	env, err := e.generate()
	if err != nil {
		e.logf("生成环境快照失败：%v", err)
		if e.env != nil {
			return e.env
		}
		return os.Environ()
	}
	e.env, e.sources, e.taken = env, sources, time.Now()
	if e.CachePath != "" {
		if data, err := json.Marshal(snapshotCache{Env: env, Sources: sources, Taken: e.taken}); err == nil {
			if err := os.MkdirAll(filepath.Dir(e.CachePath), 0o700); err == nil {
				temporary := e.CachePath + ".tmp"
				if os.WriteFile(temporary, data, 0o600) == nil {
					os.Rename(temporary, e.CachePath)
				}
			}
		}
	}
	e.logf("已生成环境快照（%d 个变量）", len(env))
	return env
}

func (e *EnvSnapshot) generate() ([]string, error) {
	timeout := e.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	script := fmt.Sprintf("printf '%%s' '%s'; env -0; printf '%%s' '%s'", snapshotBegin, snapshotEnd)
	cmd := exec.CommandContext(ctx, e.Shell, "-l", "-i", "-c", script)
	cmd.Dir = e.Home
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil && stdout.Len() == 0 {
		return nil, err
	}
	return parseSnapshot(stdout.Bytes())
}

// parseSnapshot 从混有其他输出的内容里取出两个标记之间、NUL 分隔的环境变量。
func parseSnapshot(output []byte) ([]string, error) {
	begin := bytes.LastIndex(output, []byte(snapshotBegin))
	if begin < 0 {
		return nil, fmt.Errorf("输出里没有开始标记")
	}
	rest := output[begin+len(snapshotBegin):]
	end := bytes.Index(rest, []byte(snapshotEnd))
	if end < 0 {
		return nil, fmt.Errorf("输出里没有结束标记")
	}
	var env []string
	for _, item := range bytes.Split(rest[:end], []byte{0}) {
		key, _, ok := strings.Cut(string(item), "=")
		if !ok || key == "" || snapshotExclude[key] {
			continue
		}
		env = append(env, string(item))
	}
	if len(env) == 0 {
		return nil, fmt.Errorf("快照为空")
	}
	return env, nil
}

// mergeEnv 用 overrides 覆盖 base 里的同名变量。
func mergeEnv(base []string, overrides map[string]string) []string {
	result := make([]string, 0, len(base)+len(overrides))
	for _, item := range base {
		key, _, _ := strings.Cut(item, "=")
		if _, ok := overrides[key]; !ok {
			result = append(result, item)
		}
	}
	for key, value := range overrides {
		result = append(result, key+"="+value)
	}
	return result
}
