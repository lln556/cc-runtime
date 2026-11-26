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

func TestSeedCopiesCodexCurrentVersionWithoutIdentity(t *testing.T) {
	base := t.TempDir()
	src, dst := filepath.Join(base, "src"), filepath.Join(base, "dst")
	currentRelease := ".codex/packages/standalone/releases/0.172.0-aarch64-unknown-linux-musl"
	oldRelease := ".codex/packages/standalone/releases/0.171.0-aarch64-unknown-linux-musl"

	for rel, data := range map[string]string{
		currentRelease + "/bin/codex":         "codex-new",
		currentRelease + "/codex-path/rg":     "rg",
		oldRelease + "/bin/codex":             "codex-old",
		".codex/auth.json":                    "secret",
		".codex/installation_id":              "id-1",
		".codex/config.toml":                  "model = 'x'",
		".codex/sessions/rollout.jsonl":       "history",
		".local/bin/agy":                      "agy-bin",
	} {
		path := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/home/dev/"+currentRelease, filepath.Join(src, ".codex/packages/standalone/current")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/home/dev/.codex/packages/standalone/current/bin/codex", filepath.Join(src, ".local/bin/codex")); err != nil {
		t.Fatal(err)
	}

	if err := Seed(src, dst, Options{Out: func(string) {}}); err != nil {
		t.Fatal(err)
	}

	if got, err := os.Readlink(filepath.Join(dst, ".local/bin/codex")); err != nil || got != "/home/dev/.codex/packages/standalone/current/bin/codex" {
		t.Fatalf(".local/bin/codex 链接不对：%q, %v", got, err)
	}
	if got, err := os.Readlink(filepath.Join(dst, ".codex/packages/standalone/current")); err != nil || got != "/home/dev/"+currentRelease {
		t.Fatalf("current 链接不对：%q, %v", got, err)
	}
	for _, rel := range []string{".local/bin/agy", currentRelease + "/bin/codex", currentRelease + "/codex-path/rg"} {
		if _, err := os.Stat(filepath.Join(dst, rel)); err != nil {
			t.Errorf("没有复制 %s：%v", rel, err)
		}
	}
	for _, rel := range []string{oldRelease, ".codex/auth.json", ".codex/installation_id", ".codex/config.toml", ".codex/sessions"} {
		if _, err := os.Lstat(filepath.Join(dst, rel)); err == nil {
			t.Errorf("不应复制 %s", rel)
		}
	}
}
