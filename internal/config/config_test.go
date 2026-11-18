package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zeke-chin/cc-runtime/internal/config"
)

func TestEnvFileAndEnvironment(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, ".env"), []byte("# comment\nPASSWORD=x\nWORKSPACE_DIR=\"/w s\"\nRUNTIME_DIR=/r\n"), 0o644)
	cfg := config.Load(root, map[string]string{"RUNTIME_DIR": "/override"})
	if cfg.Workspace != "/w s" || cfg.Runtime != "/override" {
		t.Fatalf("得到 %q %q", cfg.Workspace, cfg.Runtime)
	}
	if cfg.StateDir() != "/override/state" || cfg.LogDir() != "/override/logs" {
		t.Fatal(cfg.StateDir(), cfg.LogDir())
	}
	home, _ := os.UserHomeDir()
	if got := config.Load(filepath.Join(root, "missing"), map[string]string{}).Runtime; got != filepath.Join(home, ".cc-runtime") {
		t.Fatal(got)
	}
}

func TestScreenshotsIsOptional(t *testing.T) {
	root := t.TempDir()
	if got := config.Load(root, map[string]string{}).Screenshots; got != "" {
		t.Fatalf("未配置时得到 %q", got)
	}
	os.WriteFile(filepath.Join(root, ".env"), []byte("HOST_SCREENSHOTS_PATH=  \n"), 0o644)
	if got := config.Load(root, map[string]string{}).Screenshots; got != "" {
		t.Fatalf("空值得到 %q", got)
	}
	home, _ := os.UserHomeDir()
	got := config.Load(root, map[string]string{"HOST_SCREENSHOTS_PATH": "~/Pictures/shots"}).Screenshots
	if got != filepath.Join(home, "Pictures/shots") {
		t.Fatal(got)
	}
}
