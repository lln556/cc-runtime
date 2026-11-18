package seed

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSeedCopiesToolsAndCredentialsButNoIdentity(t *testing.T) {
	base := t.TempDir()
	src, dst := filepath.Join(base, "src"), filepath.Join(base, "dst")
	for rel, data := range map[string]string{
		".zshrc": "zsh", ".ssh/id_ed25519": "key", ".ssh/known_hosts": "hosts",
		".claude.json": "identity", ".claude/settings.json": "x", ".zhistory": "history",
		".local/bin/tool": "tool", ".cargo/bin/cargo": "toolchain",
	} {
		os.MkdirAll(filepath.Dir(filepath.Join(src, rel)), 0o755)
		os.WriteFile(filepath.Join(src, rel), []byte(data), 0o600)
	}
	var out []string
	if err := Seed(src, dst, Options{Out: func(s string) { out = append(out, s) }}); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{".zshrc", ".ssh/id_ed25519", ".ssh/known_hosts", ".local/bin/tool"} {
		if _, err := os.Stat(filepath.Join(dst, rel)); err != nil {
			t.Errorf("没有复制 %s", rel)
		}
	}
	for _, rel := range []string{".claude.json", ".claude", ".zhistory", ".cargo"} {
		if _, err := os.Lstat(filepath.Join(dst, rel)); err == nil {
			t.Errorf("不应出现 %s", rel)
		}
	}
	if err := Seed(src, dst, Options{Out: func(string) {}}); err == nil || !strings.Contains(err.Error(), "不为空") {
		t.Fatal(err)
	}
	if err := Seed(src, filepath.Join(src, "inner"), Options{Out: func(string) {}}); err == nil {
		t.Fatal("目标在来源里应报错")
	}
}
