// Package config 读取路径和设置：来自环境变量和仓库的 .env 文件。
package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// DefaultRoot 是安装时写入二进制的仓库路径（scripts/install.sh 用 -ldflags 设置）。
var DefaultRoot string

// Version 是安装时写入的提交号。
var Version = "dev"

// FindRoot 确定仓库目录：CC_RUNTIME_ROOT，其次是安装时写入的路径，最后从当前目录向上查找。
func FindRoot() (string, error) {
	if root := os.Getenv("CC_RUNTIME_ROOT"); root != "" {
		return root, nil
	}
	if DefaultRoot != "" && isRepo(DefaultRoot) {
		return DefaultRoot, nil
	}
	if dir, err := os.Getwd(); err == nil {
		for {
			if isRepo(dir) {
				return dir, nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	if DefaultRoot != "" {
		return "", errors.New("安装时记录的仓库 " + DefaultRoot + " 已不存在；请在仓库里重新运行 scripts/install.sh，或设置 CC_RUNTIME_ROOT")
	}
	return "", errors.New("找不到 cc-runtime 仓库：请在仓库里运行 scripts/install.sh 安装，或设置 CC_RUNTIME_ROOT")
}

func isRepo(dir string) bool {
	for _, name := range []string{"compose.yaml", "go.mod"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return false
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	return err == nil && strings.Contains(string(data), "module github.com/zeke-chin/cc-runtime")
}

// ReadEnvFile 读取 KEY=VALUE 格式的文件；文件不存在时返回空表。
func ReadEnvFile(path string) map[string]string {
	values := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return values
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		key, value, _ := strings.Cut(line, "=")
		value = strings.TrimSpace(value)
		if len(value) >= 2 && value[0] == value[len(value)-1] && (value[0] == '\'' || value[0] == '"') {
			value = value[1 : len(value)-1]
		}
		values[strings.TrimSpace(key)] = value
	}
	return values
}

// ExpandPath 展开 ~ 和环境变量。
func ExpandPath(value string) string {
	if value == "~" || strings.HasPrefix(value, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			value = home + value[1:]
		}
	}
	return filepath.Clean(os.ExpandEnv(value))
}

type Config struct {
	Root      string
	Workspace string
	Runtime   string
	Downloads string
	// Screenshots 是可选的宿主机截图目录（HOST_SCREENSHOTS_PATH），未配置时为空。
	Screenshots string
	Env         map[string]string
}

func (c Config) StateDir() string { return filepath.Join(c.Runtime, "state") }
func (c Config) LogDir() string   { return filepath.Join(c.Runtime, "logs") }

// Load 读取 root/.env，再用环境变量覆盖其中的目录设置。environ 为 nil 时使用进程环境。
func Load(root string, environ map[string]string) Config {
	if environ == nil {
		environ = map[string]string{}
		for _, item := range os.Environ() {
			key, value, _ := strings.Cut(item, "=")
			environ[key] = value
		}
	}
	values := ReadEnvFile(filepath.Join(root, ".env"))
	for _, key := range []string{"WORKSPACE_DIR", "RUNTIME_DIR", "DOWNLOADS_DIR", "HOST_SCREENSHOTS_PATH"} {
		if value, ok := environ[key]; ok {
			values[key] = value
		}
	}
	get := func(key, fallback string) string {
		if value, ok := values[key]; ok {
			return ExpandPath(value)
		}
		return ExpandPath(fallback)
	}
	cfg := Config{
		Root:      root,
		Workspace: get("WORKSPACE_DIR", "~/workspace"),
		Runtime:   get("RUNTIME_DIR", "~/.cc-runtime"),
		Downloads: get("DOWNLOADS_DIR", "~/Downloads"),
		Env:       values,
	}
	if value := strings.TrimSpace(values["HOST_SCREENSHOTS_PATH"]); value != "" {
		cfg.Screenshots = ExpandPath(value)
	}
	return cfg
}
