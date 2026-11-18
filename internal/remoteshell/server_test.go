package remoteshell

import (
	"bytes"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const testToken = "secret-token"

func init() {
	// 测试里用短的超时。
	PingInterval = 100 * time.Millisecond
	PeerTimeout = 600 * time.Millisecond
	TerminateGrace = 300 * time.Millisecond
	SignalGrace = 2 * time.Second
	DrainTimeout = 200 * time.Millisecond
}

func shell(t *testing.T) string {
	path, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("没有 bash")
	}
	return path
}

func startServer(t *testing.T) (*Server, string) {
	t.Helper()
	server := &Server{Shell: shell(t), Env: os.Environ,
		Auth: func() (map[string]string, error) { return map[string]string{testToken: "dev-test"}, nil }}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(listener)
	t.Cleanup(func() { listener.Close() })
	return server, listener.Addr().String()
}

func request(t *testing.T, command string) Request {
	return Request{Version: ProtocolVersion, Token: testToken, Cwd: t.TempDir(), Command: []byte(command)}
}

func runCommand(t *testing.T, addr string, req Request, cwdFile string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(addr, req, cwdFile, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestRunOutputAndExitCode(t *testing.T) {
	_, addr := startServer(t)
	code, stdout, stderr := runCommand(t, addr, request(t, "echo out; echo err >&2; exit 3"), "")
	if code != 3 || stdout != "out\n" || stderr != "err\n" {
		t.Fatalf("%d %q %q", code, stdout, stderr)
	}
}

func TestRunArgsAndEnv(t *testing.T) {
	_, addr := startServer(t)
	req := request(t, `printf '%s|%s|%s' "$0" "$1" "$FOO"`)
	req.Args = [][]byte{[]byte("name"), []byte("a b")}
	req.Env = map[string]string{"FOO": "bar"}
	if code, stdout, _ := runCommand(t, addr, req, ""); code != 0 || stdout != "name|a b|bar" {
		t.Fatalf("%d %q", code, stdout)
	}
}

func TestRunLargeOutput(t *testing.T) {
	_, addr := startServer(t)
	code, stdout, _ := runCommand(t, addr, request(t, "head -c 5000000 /dev/zero | tr '\\0' x"), "")
	if code != 0 || len(stdout) != 5000000 || strings.Trim(stdout, "x") != "" {
		t.Fatalf("%d %d", code, len(stdout))
	}
}

func TestRunWritesBackCwd(t *testing.T) {
	_, addr := startServer(t)
	req := request(t, "")
	sub := filepath.Join(req.Cwd, "sub")
	os.Mkdir(sub, 0o755)
	command, _, ok := RewriteCwd(`eval 'cd sub' && pwd -P >| /tmp/claude-x-cwd`)
	if !ok {
		t.Fatal("改写失败")
	}
	req.Command, req.CaptureCwd = []byte(command), true
	cwdFile := filepath.Join(t.TempDir(), "claude-x-cwd")
	if code, _, stderr := runCommand(t, addr, req, cwdFile); code != 0 {
		t.Fatalf("%d %s", code, stderr)
	}
	data, err := os.ReadFile(cwdFile)
	real, _ := filepath.EvalSymlinks(sub)
	if err != nil || strings.TrimSpace(string(data)) != real {
		t.Fatalf("%q %v", data, err)
	}
	// 命令失败时不写（和本地 `&& pwd -P` 一样）。
	os.Remove(cwdFile)
	command, _, _ = RewriteCwd(`eval 'false' && pwd -P >| /tmp/claude-x-cwd`)
	req.Command = []byte(command)
	if code, _, _ := runCommand(t, addr, req, cwdFile); code != 1 {
		t.Fatalf("退出码 %d", code)
	}
	if _, err := os.Stat(cwdFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("失败的命令不应写回 cwd")
	}
}

func TestRunRejects(t *testing.T) {
	_, addr := startServer(t)
	req := request(t, "true")
	req.Token = "wrong"
	if code, _, stderr := runCommand(t, addr, req, ""); code != 2 || !strings.Contains(stderr, "认证失败") {
		t.Fatalf("%d %q", code, stderr)
	}
	req = request(t, "true")
	req.Version = 99
	if code, _, stderr := runCommand(t, addr, req, ""); code != 2 || !strings.Contains(stderr, "协议版本") {
		t.Fatalf("%d %q", code, stderr)
	}
	if code, _, stderr := runCommand(t, "127.0.0.1:1", request(t, "true"), ""); code != 255 || !strings.Contains(stderr, "无法连接") {
		t.Fatalf("%d %q", code, stderr)
	}
}

func TestMissingCwdIsRejectedWithHelp(t *testing.T) {
	server, addr := startServer(t)
	server.SharedDirs = []string{"/Users/x/workspace", "/Users/x/Downloads"}
	req := request(t, "cd /tmp && touch should-not-exist")
	req.Cwd = "/home/dev-does-not-exist"
	code, stdout, stderr := runCommand(t, addr, req, "")
	if code != 1 || stdout != "" {
		t.Fatalf("%d %q", code, stdout)
	}
	for _, want := range []string{"命令没有执行", "/home/dev-does-not-exist", "cd 也没有用",
		"cd /Users/x/workspace/<项目> && claude", "/Users/x/workspace、/Users/x/Downloads"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("提示里缺少 %q：%s", want, stderr)
		}
	}
	if _, err := os.Stat("/tmp/should-not-exist"); err == nil {
		t.Fatal("命令不应执行")
	}
}

// startRaw 发出请求但不读，模拟客户端随后消失；命令把自己和后台子进程的 pid 写到文件里。
func startRaw(t *testing.T, addr, command string) (net.Conn, []int) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	pids := filepath.Join(t.TempDir(), "pids")
	req := request(t, "echo $$ > "+pids+".tmp; "+command+" & echo $! >> "+pids+".tmp; mv "+pids+".tmp "+pids+"; wait")
	if err := NewFrameWriter(conn).WriteJSON(FrameRequest, req); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(pids); err == nil {
			var result []int
			for _, field := range strings.Fields(string(data)) {
				pid, _ := strconv.Atoi(field)
				result = append(result, pid)
			}
			return conn, result
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("命令没有启动")
	return nil, nil
}

func waitGone(t *testing.T, pids []int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		alive := false
		for _, pid := range pids {
			// 已退出但未回收的进程也算结束。
			if data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
				if !strings.Contains(string(data), ") Z ") {
					alive = true
				}
			} else if syscall.Kill(pid, 0) == nil && !fileExists("/proc") {
				alive = true
			}
		}
		if !alive {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("进程 %v 没有被清理", pids)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestDisconnectKillsProcessGroup(t *testing.T) {
	_, addr := startServer(t)
	conn, pids := startRaw(t, addr, "sleep 300")
	conn.Close()
	waitGone(t, pids, 3*time.Second)
}

func TestHeartbeatTimeoutKillsProcessGroup(t *testing.T) {
	_, addr := startServer(t)
	conn, pids := startRaw(t, addr, "sleep 300")
	defer conn.Close()
	// 连接还在，但客户端不再发任何帧（例如中间的 tunnel 已经断了）。
	waitGone(t, pids, PeerTimeout+3*time.Second)
}

func TestTermIgnoringCommandGetsKilled(t *testing.T) {
	_, addr := startServer(t)
	conn, pids := startRaw(t, addr, "trap '' TERM; sleep 300")
	conn.Close()
	waitGone(t, pids, TerminateGrace+3*time.Second)
}

func TestSignalIsForwarded(t *testing.T) {
	_, addr := startServer(t)
	done := make(chan int, 1)
	go func() {
		code, _, _ := runCommand(t, addr, request(t, "trap 'exit 7' TERM; sleep 30 & wait"), "")
		done <- code
	}()
	time.Sleep(500 * time.Millisecond)
	syscall.Kill(os.Getpid(), syscall.SIGTERM) // run() 已经接管了这个信号
	select {
	case code := <-done:
		if code != 7 {
			t.Fatalf("退出码 %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("信号没有结束命令")
	}
}

func TestBackgroundChildDoesNotBlockExit(t *testing.T) {
	_, addr := startServer(t)
	started := time.Now()
	// 后台进程继承了 stdout；命令本身结束后应该在 DrainTimeout 左右返回，不等后台进程。
	code, stdout, _ := runCommand(t, addr, request(t, "sleep 3 & echo done"), "")
	if code != 0 || stdout != "done\n" || time.Since(started) > 2*time.Second {
		t.Fatalf("%d %q %s", code, stdout, time.Since(started))
	}
}

func TestShutdownAbortsCommands(t *testing.T) {
	server, addr := startServer(t)
	done := make(chan []string, 1)
	go func() {
		code, _, stderr := runCommand(t, addr, request(t, "sleep 30"), "")
		done <- []string{strconv.Itoa(code), stderr}
	}()
	time.Sleep(500 * time.Millisecond)
	server.Shutdown("服务正在重启")
	select {
	case result := <-done:
		if result[0] != "255" || !strings.Contains(result[1], "服务正在重启") {
			t.Fatalf("%v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("命令没有被中止")
	}
}

func TestEnvSnapshot(t *testing.T) {
	home := t.TempDir()
	rc := filepath.Join(home, ".zshrc")
	os.WriteFile(rc, nil, 0o644)
	// 用一个假的 shell：输出噪声和带标记的环境。
	fake := filepath.Join(t.TempDir(), "fake-zsh")
	os.WriteFile(fake, []byte("#!/bin/sh\necho noise\nprintf '%s' '"+snapshotBegin+"'; printf 'PATH=/opt/x\\000N='; cat \""+
		filepath.Join(home, "n")+"\"; printf '\\000PWD=/y\\000'; printf '%s' '"+snapshotEnd+"'\n"), 0o755)
	os.WriteFile(filepath.Join(home, "n"), []byte("1"), 0o644)
	cache := filepath.Join(t.TempDir(), "env.json")
	snapshot := &EnvSnapshot{Shell: fake, Home: home, CachePath: cache}
	if got := strings.Join(snapshot.Get(), " "); got != "PATH=/opt/x N=1" {
		t.Fatalf("得到 %q", got)
	}
	// 配置文件没变：用缓存（即使环境已经不同）。
	os.WriteFile(filepath.Join(home, "n"), []byte("2"), 0o644)
	if got := strings.Join((&EnvSnapshot{Shell: fake, Home: home, CachePath: cache}).Get(), " "); got != "PATH=/opt/x N=1" {
		t.Fatalf("应使用缓存，得到 %q", got)
	}
	// 配置文件改了：重新生成。
	future := time.Now().Add(time.Minute)
	os.Chtimes(rc, future, future)
	if got := strings.Join(snapshot.Get(), " "); got != "PATH=/opt/x N=2" {
		t.Fatalf("应重新生成，得到 %q", got)
	}
}
