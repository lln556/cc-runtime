// Package testutil 是各包测试共用的工具。
package testutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/zeke-chin/cc-runtime/internal/config"
)

// RepoRoot 是本仓库的根目录。
func RepoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// MakeConfig 建一份工作区和运行目录都在 base 下的配置；root 为空时用 base/repo。
func MakeConfig(t *testing.T, base, root string) config.Config {
	t.Helper()
	if root == "" {
		root = filepath.Join(base, "repo")
	}
	runtimeDir := filepath.Join(base, "runtime")
	for _, path := range []string{filepath.Join(base, "mac"), runtimeDir, root} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return config.Config{Root: root, Workspace: filepath.Join(base, "mac"), Runtime: runtimeDir,
		Downloads: filepath.Join(base, "downloads")}
}
