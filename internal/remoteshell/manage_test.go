package remoteshell

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeke-chin/cc-runtime/internal/testutil"
)

func TestMachinesAndPort(t *testing.T) {
	cfg := testutil.MakeConfig(t, t.TempDir(), "")
	cfg.Env = map[string]string{"REMOTE_SHELL_MACHINES": " dev-a, dev-b,,dev-a "}
	if got := strings.Join(Machines(cfg), ","); got != "dev-a,dev-b" {
		t.Fatalf("得到 %q", got)
	}
	if port, err := Port(cfg); err != nil || port != DefaultPort {
		t.Fatalf("%d %v", port, err)
	}
	cfg.Env["REMOTE_SHELL_PORT"] = "abc"
	if _, err := Port(cfg); err == nil {
		t.Fatal("无效端口应该报错")
	}
}

func TestPrepareMachineKeepsToken(t *testing.T) {
	cfg := testutil.MakeConfig(t, t.TempDir(), "")
	if _, err := PrepareMachine(cfg, "dev-a", "172.30.0.4"); err == nil {
		t.Fatal("没有开发机二进制时应该报错")
	}
	os.MkdirAll(BinSource(cfg), 0o755)
	os.WriteFile(filepath.Join(BinSource(cfg), LinuxBinary), nil, 0o755)
	first, err := PrepareMachine(cfg, "dev-a", "172.30.0.4")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Token) != 64 || first.Addr != "172.30.0.4:10024" || first.TmpDir != TmpDir(cfg, "dev-a") {
		t.Fatalf("%+v", first)
	}
	for _, path := range []string{MachineDir(cfg, "dev-a"), TmpDir(cfg, "dev-a"), ClientConfigPath(cfg, "dev-a")} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s 权限不对：%v %v", path, info.Mode(), err)
		}
	}
	second, err := PrepareMachine(cfg, "dev-a", "172.30.0.2")
	if err != nil || second.Token != first.Token || second.Addr != "172.30.0.2:10024" {
		t.Fatalf("%+v %v", second, err)
	}
	// token 按启用的机器读取。
	os.WriteFile(filepath.Join(cfg.Root, ".env"), []byte("REMOTE_SHELL_MACHINES=dev-a\n"), 0o644)
	tokens, _ := Tokens(cfg)
	if tokens[first.Token] != "dev-a" || len(tokens) != 1 {
		t.Fatalf("%v", tokens)
	}
	os.WriteFile(filepath.Join(cfg.Root, ".env"), []byte("REMOTE_SHELL_MACHINES=\n"), 0o644)
	if tokens, _ := Tokens(cfg); len(tokens) != 0 {
		t.Fatalf("停用后不应再接受：%v", tokens)
	}
}
