package status

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeke-chin/cc-runtime/internal/testutil"
	"github.com/zeke-chin/cc-runtime/internal/ui"
)

func TestContainersSectionShowsExitedNetworkInit(t *testing.T) {
	base := t.TempDir()
	cfg := testutil.MakeConfig(t, base, filepath.Join(base, "cc-runtime"))
	bin := filepath.Join(base, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeDocker := `#!/bin/sh
case "$1" in
    ps)
        printf '%s\n' '{"Names":"cc-runtime-dev-pennsylvania-1","State":"running","Status":"Up 5 minutes","Labels":"com.docker.compose.service=dev-pennsylvania"}'
        printf '%s\n' '{"Names":"cc-runtime-dev-pennsylvania-network-init-1","State":"exited","Status":"Exited (0) 5 minutes ago","Labels":"com.docker.compose.service=dev-pennsylvania-network-init"}'
        ;;
    compose)
        printf 'dev-pennsylvania\ndev-pennsylvania-network-init\n'
        ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(fakeDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	ui.SetColor(true)
	defer ui.SetColor(false)

	rendered := strings.Join(ContainersSection(cfg).Lines(120), "\n")
	if !strings.Contains(rendered, "dev-pennsylvania-network-init") {
		t.Fatalf("已退出的 network-init 未显示在状态面板中：\n%s", rendered)
	}
	if !strings.Contains(rendered, Bad.S("Exited (0) 5 minutes ago")) {
		t.Fatalf("已退出的 network-init 应标为异常状态：\n%s", rendered)
	}
	if !strings.Contains(rendered, OK.S("Up 5 minutes")) {
		t.Fatalf("运行中的容器应标为正常状态：\n%s", rendered)
	}
}
