package remoteshell

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	switch os.Getenv("CC_TEST_CODEX_REEXEC") {
	case "shell":
		os.Exit(CodexShellMain(append([]string{os.Getenv("CC_TEST_CODEX_ARGV0")}, os.Args[1:]...)))
	case "session-start":
		os.Exit(CodexSessionStartMain(os.Stdin, os.Stdout))
	case "client":
		os.Exit(ClientMain(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func codexTestBinary(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func cleanCodexEnv(extra ...string) []string {
	drop := map[string]bool{
		CodexRemoteEnv: true, LocalSwitch: true, testLocalZshEnv: true, ConfigEnv: true,
		"SHELL": true, "CODEX_CI": true, "PWD": true,
	}
	env := make([]string, 0, len(os.Environ())+len(extra))
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if !drop[key] {
			env = append(env, item)
		}
	}
	return append(env, extra...)
}

func runTestBinary(t *testing.T, reexec, argv0 string, args, env []string, dir string) (int, string) {
	t.Helper()
	cmd := exec.Command(codexTestBinary(t), args...)
	cmd.Dir = dir
	cmd.Env = append(append([]string(nil), env...), "CC_TEST_CODEX_REEXEC="+reexec, "CC_TEST_CODEX_ARGV0="+argv0)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("运行测试二进制失败：%v\n%s", err, out)
	}
	return exitErr.ExitCode(), string(out)
}

func runCodexShell(t *testing.T, argv0 string, args, env []string, dir string) (int, string) {
	t.Helper()
	return runTestBinary(t, "shell", argv0, args, env, dir)
}

func runClient(t *testing.T, args, env []string, dir string) (int, string) {
	t.Helper()
	return runTestBinary(t, "client", "", args, env, dir)
}

func startRecordingServer(t *testing.T) (string, <-chan string) {
	t.Helper()
	server := &Server{Shell: shell(t), Env: os.Environ,
		Auth: func() (map[string]string, error) { return map[string]string{testToken: "dev-test"}, nil }}
	seen := make(chan string, 8)
	server.State = func(active []Active) {
		for _, item := range active {
			select {
			case seen <- item.Cwd:
			default:
			}
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(listener)
	t.Cleanup(func() { listener.Close() })
	return listener.Addr().String(), seen
}

func waitRecordedCwd(t *testing.T, seen <-chan string) string {
	t.Helper()
	select {
	case cwd := <-seen:
		return cwd
	case <-time.After(5 * time.Second):
		t.Fatal("服务没有收到命令")
		return ""
	}
}

func codexZsh(t *testing.T) string {
	t.Helper()
	for _, candidate := range []string{"/bin/zsh", "/usr/bin/zsh"} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if path, err := exec.LookPath("zsh"); err == nil {
		return path
	}
	t.Skip("需要 zsh")
	return ""
}

func TestCodexShellStaysLocalAndKeepsLoginShell(t *testing.T) {
	zsh := codexZsh(t)
	command := `printf '%s|%s|%s\n' "$([[ -o login ]] && echo login || echo nologin)" "$SHELL" "${CC_CODEX_REMOTE_SHELL-unset}"`
	code, out := runCodexShell(t, "-zsh", []string{"-f", "-c", command},
		cleanCodexEnv(testLocalZshEnv+"="+zsh, "SHELL="+CodexShellPath, CodexRemoteEnv+"=0"), t.TempDir())
	if code != 0 {
		t.Fatalf("退出码 %d：%s", code, out)
	}
	if want := "login|" + zsh + "|unset\n"; out != want {
		t.Fatalf("得到 %q，期望 %q", out, want)
	}
}

func TestCodexShellRoutesRemoteCommands(t *testing.T) {
	_, addr := startServer(t)
	client := ClientConfig{Machine: "dev-test", Addr: addr, Token: testToken, TmpDir: "/Users/x/tmp"}
	configPath := filepath.Join(t.TempDir(), "client.json")
	data, _ := json.Marshal(client)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	tmp := t.TempDir()
	realDir := filepath.Join(tmp, "real-workspace")
	linkDir := filepath.Join(tmp, "link-workspace")
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

	command := `printf '%s|%s|%s\n' "$PWD" "$CODEX_CI" "${CC_CODEX_REMOTE_SHELL-unset}"`
	code, out := runCodexShell(t, CodexShellPath, []string{"-lc", command},
		cleanCodexEnv(CodexRemoteEnv+"=1", "CODEX_CI=1", ConfigEnv+"="+configPath, "PWD="+linkDir), linkDir)
	if code != 0 {
		t.Fatalf("退出码 %d：%s", code, out)
	}
	if want := wantReal + "|1|unset\n"; out != want {
		t.Fatalf("得到 %q，期望 %q", out, want)
	}
}

func TestClientResolvesSymlinkedCwd(t *testing.T) {
	addr, seen := startRecordingServer(t)
	client := ClientConfig{Machine: "dev-test", Addr: addr, Token: testToken}
	configPath := filepath.Join(t.TempDir(), "client.json")
	data, _ := json.Marshal(client)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	tmp := t.TempDir()
	realDir := filepath.Join(tmp, "real-workspace")
	linkDir := filepath.Join(tmp, "link-workspace")
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

	code, out := runClient(t, []string{"-c", `printf 'ok\n'`},
		cleanCodexEnv(ConfigEnv+"="+configPath, "PWD="+linkDir), linkDir)
	if code != 0 || out != "ok\n" {
		t.Fatalf("退出码 %d：%s", code, out)
	}
	if got := waitRecordedCwd(t, seen); got != wantReal {
		t.Fatalf("服务收到的工作目录 %q，期望 %q", got, wantReal)
	}
}

func TestCodexShellKeepsLocalCallsInContainer(t *testing.T) {
	zsh := codexZsh(t)
	base := cleanCodexEnv(testLocalZshEnv+"="+zsh, CodexRemoteEnv+"=1")
	dir := t.TempDir()

	code, out := runCodexShell(t, CodexShellPath, []string{"-c", `printf '%s\n' "${CC_CODEX_REMOTE_SHELL-unset}"`},
		append(append([]string(nil), base...), LocalSwitch+"=1"), dir)
	if code != 0 || out != "unset\n" {
		t.Fatalf("本地开关：退出码 %d，输出 %q", code, out)
	}

	script := filepath.Join(dir, "script.sh")
	if err := os.WriteFile(script, []byte(`printf 'SCRIPT|%s\n' "${CC_CODEX_REMOTE_SHELL-unset}"`), 0o755); err != nil {
		t.Fatal(err)
	}
	code, out = runCodexShell(t, CodexShellPath, []string{script}, base, dir)
	if code != 0 || out != "SCRIPT|unset\n" {
		t.Fatalf("脚本调用：退出码 %d，输出 %q", code, out)
	}
}

func TestCodexSessionStartFormatsContextWithoutCorruptingPaths(t *testing.T) {
	saved := ProbeCommand
	t.Cleanup(func() { ProbeCommand = saved })
	ProbeCommand = `printf '%s\n' "$PWD" 24F74 arm64`

	_, addr := startServer(t)
	client := ClientConfig{
		Addr:       addr,
		Token:      testToken,
		TmpDir:     "/Users/x/.claude-shared/tmp",
		SharedDirs: []string{"/Users/x/claude-projects"},
		Container:  "cc-runtime-dev-a-1",
	}
	configPath := filepath.Join(t.TempDir(), "client.json")
	data, _ := json.Marshal(client)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ConfigEnv, configPath)

	tmp := t.TempDir()
	realDir := filepath.Join(tmp, "claude-real-project")
	linkDir := filepath.Join(tmp, "link-project")
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

	input, _ := json.Marshal(map[string]string{"cwd": linkDir, "source": "startup"})
	var out bytes.Buffer
	if code := CodexSessionStartMain(bytes.NewReader(input), &out); code != 0 {
		t.Fatalf("退出码 %d", code)
	}
	var parsed struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil || parsed.HookSpecificOutput.HookEventName != "SessionStart" {
		t.Fatalf("输出不是合法的 hook JSON：%v %s", err, out.String())
	}
	context := parsed.HookSpecificOutput.AdditionalContext
	for _, want := range []string{
		"macOS " + wantReal,
		"/Users/x/claude-projects",
		"/Users/x/.claude-shared/tmp",
		"Codex",
		"shell 命令",
		"在 shell 工具里",
		"docker exec cc-runtime-dev-a-1",
	} {
		if !strings.Contains(context, want) {
			t.Errorf("缺少 %q：\n%s", want, context)
		}
	}
	for _, unwanted := range []string{"Claude Code", "Bash 命令", "在 Bash 里"} {
		if strings.Contains(context, unwanted) {
			t.Errorf("残留 %q：\n%s", unwanted, context)
		}
	}
}
