package remoteshell

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func antigravityRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "antigravity"))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func stageAntigravityDir(t *testing.T) string {
	t.Helper()
	src := antigravityRoot(t)
	dst := filepath.Join(t.TempDir(), "antigravity")
	if err := os.MkdirAll(filepath.Join(dst, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"agy", "setup", "session-start", "bin/bash", "GEMINI.md"} {
		data, err := os.ReadFile(filepath.Join(src, rel))
		if err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o755)
		if rel == "GEMINI.md" {
			mode = 0o644
		}
		if err := os.WriteFile(filepath.Join(dst, rel), data, mode); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

func TestAntigravitySetupMergesHooksAndPreservesUserFiles(t *testing.T) {
	dir := stageAntigravityDir(t)
	home := filepath.Join(t.TempDir(), "home")
	cfgDir := filepath.Join(home, ".gemini", "config")
	t3CfgDir := filepath.Join(home, ".t3", "userdata", "providers", "antigravity", "profile-1", "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(t3CfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	existingHooks := `{"herdr":{"PreInvocation":[{"type":"command","command":"herdr-hook","timeout":10}]}}`
	if err := os.WriteFile(filepath.Join(cfgDir, "hooks.json"), []byte(existingHooks), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(filepath.Join(dir, "setup"), home)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("setup 失败：%v\n%s", err, out)
	}

	target, err := os.Readlink(filepath.Join(home, ".gemini", "GEMINI.md"))
	if err != nil || target != filepath.Join(dir, "GEMINI.md") {
		t.Fatalf("GEMINI.md 链接不对：%q, %v", target, err)
	}

	raw, err := os.ReadFile(filepath.Join(cfgDir, "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var hooks map[string]any
	if err := json.Unmarshal(raw, &hooks); err != nil {
		t.Fatal(err)
	}
	if _, ok := hooks["herdr"]; !ok {
		t.Fatalf("原有的 herdr hook 丢失：%s", raw)
	}
	if !strings.Contains(string(raw), filepath.Join(dir, "session-start")) {
		t.Fatalf("未写入 remote-shell SessionStart hook：%s", raw)
	}

	if _, err := os.Readlink(filepath.Join(t3CfgDir, "hooks.json")); err != nil {
		t.Fatalf("T3 provider hooks.json 未建立链接：%v", err)
	}

	// 使用者若把 GEMINI.md 换成了普通文件，setup 不覆盖它。
	geminiPath := filepath.Join(home, ".gemini", "GEMINI.md")
	os.Remove(geminiPath)
	os.WriteFile(geminiPath, []byte("custom-rules"), 0o644)
	if out, err := exec.Command(filepath.Join(dir, "setup"), home).CombinedOutput(); err != nil {
		t.Fatalf("二次 setup 失败：%v\n%s", err, out)
	}
	if got, _ := os.ReadFile(geminiPath); string(got) != "custom-rules" {
		t.Fatalf("覆盖了使用者的 GEMINI.md：%s", got)
	}
}

func TestAntigravityBashShimRoutingAndZshCompat(t *testing.T) {
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skip("需要 zsh")
	}
	dir := stageAntigravityDir(t)
	tmp := t.TempDir()

	// 用一个记录调用并转交真实 zsh 的桩代替 remote-shell/zsh，同时验证 zsh 兼容选项生效。
	fakeRemote := filepath.Join(tmp, "remote-zsh")
	fakeScript := "#!/bin/sh\nprintf 'REMOTE|' \nexec zsh \"$@\"\n"
	if err := os.WriteFile(fakeRemote, []byte(fakeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeLocal := filepath.Join(tmp, "local-bash")
	localScript := "#!/bin/sh\nprintf 'LOCAL|%s\\n' \"$PATH\"\n"
	if err := os.WriteFile(fakeLocal, []byte(localScript), 0o755); err != nil {
		t.Fatal(err)
	}

	shim := filepath.Join(dir, "bin", "bash")
	baseEnv := append(os.Environ(),
		"PATH="+filepath.Join(dir, "bin")+":"+os.Getenv("PATH"),
		"REMOTE_SHELL_ZSH="+fakeRemote,
		"REMOTE_SHELL_LOCAL_BASH="+fakeLocal,
	)

	// 1. `-c` 命令走 remote-shell，未匹配通配符、行尾注释、位置参数都正常。
	cmd := exec.Command(shim, "-c", "printf '%s|%s|%s\n' *.no_such_ext_cc \"$0\" \"$1\" # comment", "arg0", "arg1")
	cmd.Dir = tmp
	cmd.Env = append(append([]string(nil), baseEnv...), "REMOTE_SHELL_LOCAL=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("转发执行失败：%v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "REMOTE|*.no_such_ext_cc|arg0|arg1" {
		t.Fatalf("转发输出不对：%q", got)
	}

	// 2. REMOTE_SHELL_LOCAL=1 时回退到本地 bash，且 PATH 里已去掉垫片目录。
	cmd = exec.Command(shim, "-c", "echo local")
	cmd.Env = append(append([]string(nil), baseEnv...), "REMOTE_SHELL_LOCAL=1")
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("本地开关回退失败：%v\n%s", err, out)
	}
	if !strings.HasPrefix(string(out), "LOCAL|") || strings.Contains(string(out), filepath.Join(dir, "bin")) {
		t.Fatalf("本地回退未剥离垫片目录：%q", out)
	}

	// 3. 不带 `-c`（例如执行脚本文件）留在容器本地。
	cmd = exec.Command(shim, "script.sh")
	cmd.Env = append(append([]string(nil), baseEnv...), "REMOTE_SHELL_LOCAL=")
	out, err = cmd.CombinedOutput()
	if err != nil || !strings.HasPrefix(string(out), "LOCAL|") {
		t.Fatalf("非 -c 调用未走本地：%v %q", err, out)
	}

	// 4. 容器侧 hook 命令留在本地。
	cmd = exec.Command(shim, "-c", "bash '/home/dev/.gemini/config/hooks/herdr-agent-state.sh' session")
	cmd.Env = append(append([]string(nil), baseEnv...), "REMOTE_SHELL_LOCAL=")
	out, err = cmd.CombinedOutput()
	if err != nil || !strings.HasPrefix(string(out), "LOCAL|") {
		t.Fatalf("容器侧 hook 未走本地：%v %q", err, out)
	}

	// 5. 经符号链接工作目录（如 /home/dev/workspace/...）启动时，自动把 $PWD 解析为物理真实路径。
	realWorkspace := filepath.Join(tmp, "real-workspace")
	linkWorkspace := filepath.Join(tmp, "link-workspace")
	if err := os.MkdirAll(realWorkspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realWorkspace, linkWorkspace); err != nil {
		t.Fatal(err)
	}
	wantReal, err := filepath.EvalSymlinks(realWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(shim, "-c", "printf '%s\\n' \"$PWD\"")
	cmd.Dir = linkWorkspace
	cmd.Env = append(append([]string(nil), baseEnv...), "REMOTE_SHELL_LOCAL=", "PWD="+linkWorkspace)
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("符号链接目录执行失败：%v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "REMOTE|"+wantReal {
		t.Fatalf("符号链接目录未解析为物理路径：得到 %q，期望 %q", got, "REMOTE|"+wantReal)
	}
}

func TestAntigravityWrapperFindsRealBinaryWithoutRecursion(t *testing.T) {
	dir := stageAntigravityDir(t)
	home := filepath.Join(t.TempDir(), "home")
	localBin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(localBin, 0o755); err != nil {
		t.Fatal(err)
	}
	realAgy := filepath.Join(localBin, "agy")
	script := "#!/bin/sh\nprintf '%s|%s\\n' \"$(command -v bash)\" \"$*\"\n"
	if err := os.WriteFile(realAgy, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(filepath.Join(dir, "agy"), "chat", "--help")
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"PATH="+dir+":"+localBin+":"+os.Getenv("PATH"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("agy 包装器失败：%v\n%s", err, out)
	}
	want := filepath.Join(dir, "bin", "bash") + "|chat --help"
	if got := strings.TrimSpace(string(out)); got != want {
		t.Fatalf("包装器输出 %q，期望 %q", got, want)
	}
}

func TestAntigravitySessionStartAdapter(t *testing.T) {
	dir := stageAntigravityDir(t)
	probe := filepath.Join(t.TempDir(), "probe")
	probeOut := `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"本会话的 Bash 命令经 remote-shell 在宿主 Mac 上执行：macOS 15.5（24F74），arm64，zsh。系统信息里的 Platform、OS Version 描述的是运行 Claude Code 的 Linux 容器，不是命令执行的地方。\nClaude Code 的临时目录（scratchpad 在其中）：/tmp/shared，两侧路径相同。\n运行 Claude Code 的容器：cc-dev；需要容器内的环境时，在 Bash 里用 docker exec cc-dev sh -c '…'。"}}`
	stdinRecord := filepath.Join(t.TempDir(), "probe-stdin.json")
	probeScript := "#!/bin/sh\ncat > '" + stdinRecord + "'\nprintf '%s\\n' '" + strings.ReplaceAll(probeOut, "'", "'\\''") + "'\n"
	if err := os.WriteFile(probe, []byte(probeScript), 0o755); err != nil {
		t.Fatal(err)
	}

	realDir := filepath.Join(t.TempDir(), "real-project")
	linkDir := filepath.Join(t.TempDir(), "link-project")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}
	wantReal, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(filepath.Join(dir, "session-start"))
	cmd.Stdin = strings.NewReader(`{"cwd":"` + linkDir + `"}`)
	cmd.Env = append(os.Environ(), "REMOTE_SHELL_SESSION_START="+probe)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("session-start 失败：%v\n%s", err, out)
	}
	var parsed struct {
		InjectSteps []struct {
			SystemMessage struct {
				SystemMessage string `json:"systemMessage"`
			} `json:"systemMessage"`
		} `json:"injectSteps"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil || len(parsed.InjectSteps) != 1 {
		t.Fatalf("输出格式错误：%v\n%s", err, out)
	}
	msg := parsed.InjectSteps[0].SystemMessage.SystemMessage
	if !strings.Contains(msg, "run_command 命令经 remote-shell 在宿主 Mac 上执行") ||
		!strings.Contains(msg, "Antigravity (agy)") ||
		strings.Contains(msg, "Claude Code") {
		t.Fatalf("消息替换不对：%s", msg)
	}
	recorded, err := os.ReadFile(stdinRecord)
	if err != nil || !strings.Contains(string(recorded), wantReal) {
		t.Fatalf("session-start 未将符号链接 cwd 解析为真实路径 %q：%s", wantReal, recorded)
	}
}
