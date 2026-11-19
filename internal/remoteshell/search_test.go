package remoteshell

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runZsh 用 zsh 执行 SearchFunctions(dir) 加 command，PATH 只有 path。
func runZsh(t *testing.T, dir, path, command string) string {
	t.Helper()
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("没有 zsh")
	}
	cmd := exec.Command(zsh, "-f", "-c", SearchFunctions(dir)+command)
	cmd.Env = []string{"PATH=" + path, "HOME=" + t.TempDir()}
	cmd.Dir = t.TempDir()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v：%s", err, out)
	}
	return string(out)
}

// fakeTools 建一个目录，里面的 names 都是打印自己名字和参数的脚本。
func fakeTools(t *testing.T, names ...string) string {
	dir := filepath.Join(t.TempDir(), "search tools") // 带空格，检查引号
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		script := "#!/bin/sh\necho \"" + name + " $*\"\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestSearchFunctionsUseTools(t *testing.T) {
	dir := fakeTools(t, "ugrep", "bfs")
	out := runZsh(t, dir, "/usr/bin:/bin", "grep -rn foo .; find . -name x")
	wantGrep := "ugrep -G --ignore-files --hidden -I --exclude-dir=.git --exclude-dir=.svn --exclude-dir=.hg " +
		"--exclude-dir=.bzr --exclude-dir=.jj --exclude-dir=.sl -rn foo .\n"
	wantFind := "bfs -S dfs -regextype findutils-default . -name x\n"
	if out != wantGrep+wantFind {
		t.Fatalf("输出：%q", out)
	}
}

func TestSearchFunctionsFallBack(t *testing.T) {
	dir := fakeTools(t, "ugrep", "bfs")
	// ugrep 不兼容的参数交给系统的 grep。
	if out := runZsh(t, dir, "/usr/bin:/bin", "printf 'a\\n' | grep -Z a; true"); strings.Contains(out, "ugrep") {
		t.Fatalf("-Z 应交给系统的 grep：%q", out)
	}
	// 目录里没有程序时退回系统命令。
	empty := fakeTools(t)
	if out := runZsh(t, empty, "/usr/bin:/bin", "printf 'a\\n' | grep a; find . -maxdepth 0"); out != "a\n.\n" {
		t.Fatalf("应退回系统命令：%q", out)
	}
}

func TestSearchFunctionsRg(t *testing.T) {
	withRg := fakeTools(t, "rg")
	if out := runZsh(t, withRg, "/usr/bin:/bin", "whence -w rg; rg foo"); out != "rg: function\nrg foo\n" {
		t.Fatalf("PATH 里没有 rg、目录里有时应定义函数：%q", out)
	}
	if out := runZsh(t, fakeTools(t), "/usr/bin:/bin", "whence -w rg || true"); out != "rg: none\n" {
		t.Fatalf("目录里没有 rg 时不应定义函数：%q", out)
	}
	bin := fakeTools(t, "rg")
	if out := runZsh(t, withRg, bin+":/usr/bin:/bin", "whence -w rg"); out != "rg: command\n" {
		t.Fatalf("PATH 里有 rg 时不应定义函数：%q", out)
	}
}

func TestSearchFunctionsUg(t *testing.T) {
	dir := fakeTools(t, "ug")
	if out := runZsh(t, dir, "/usr/bin:/bin", "whence -w ug; ug foo"); out != "ug: function\nug foo\n" {
		t.Fatalf("PATH 里没有 ug、目录里有时应定义函数：%q", out)
	}
	if out := runZsh(t, dir, fakeTools(t, "ug")+":/usr/bin:/bin", "whence -w ug"); out != "ug: command\n" {
		t.Fatalf("PATH 里有 ug 时不应定义函数：%q", out)
	}
}

func TestServerPrependsPreamble(t *testing.T) {
	server, addr := startServer(t)
	server.Preamble = "greet() { echo hi; }\n"
	if code, stdout, _ := runCommand(t, addr, request(t, "greet"), ""); code != 0 || stdout != "hi\n" {
		t.Fatalf("%d %q", code, stdout)
	}
}
