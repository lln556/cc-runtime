package remoteshell

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeke-chin/cc-runtime/internal/testutil"
)

func TestPort(t *testing.T) {
	cfg := testutil.MakeConfig(t, t.TempDir(), "")
	cfg.Env = map[string]string{}
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
	// token 按已准备的机器读取；删掉机器目录就不再接受。
	if got := strings.Join(PreparedMachines(cfg), ","); got != "dev-a" {
		t.Fatalf("得到 %q", got)
	}
	tokens, _ := Tokens(cfg)
	if tokens[first.Token] != "dev-a" || len(tokens) != 1 {
		t.Fatalf("%v", tokens)
	}
	os.RemoveAll(MachineDir(cfg, "dev-a"))
	if tokens, _ := Tokens(cfg); len(tokens) != 0 {
		t.Fatalf("删除后不应再接受：%v", tokens)
	}
}
