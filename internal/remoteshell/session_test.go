package remoteshell

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sessionStart 以给定的 client.json 运行 hook，返回注入的上下文。
func sessionStart(t *testing.T, client ClientConfig, cwd string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "client.json")
	data, _ := json.Marshal(client)
	os.WriteFile(path, data, 0o600)
	t.Setenv(ConfigEnv, path)
	input, _ := json.Marshal(map[string]string{"cwd": cwd, "source": "startup", "hook_event_name": "SessionStart"})
	var out bytes.Buffer
	if code := SessionStartMain(bytes.NewReader(input), &out); code != 0 {
		t.Fatalf("退出码 %d", code)
	}
	var result struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.HookSpecificOutput.HookEventName != "SessionStart" {
		t.Fatalf("输出不是合法的 hook JSON：%v %s", err, out.String())
	}
	return result.HookSpecificOutput.AdditionalContext
}

func TestSessionStartDescribesTheMac(t *testing.T) {
	saved := ProbeCommand
	t.Cleanup(func() { ProbeCommand = saved })
	ProbeCommand = `printf '%s\n' 15.5 24F74 arm64`
	_, addr := startServer(t)
	client := ClientConfig{Addr: addr, Token: testToken, TmpDir: "/Users/x/.cc-runtime/remote-shell/dev-a/tmp",
		SharedDirs: []string{"/Users/x/workspace", "/Users/x/Downloads"}, Container: "cc-runtime-dev-a-1"}
	text := sessionStart(t, client, t.TempDir())
	for _, want := range []string{"macOS 15.5（24F74），arm64", "Platform、OS Version", "/Users/x/workspace、/Users/x/Downloads",
		"/Users/x/.cc-runtime/remote-shell/dev-a/tmp", "docker exec cc-runtime-dev-a-1"} {
		if !strings.Contains(text, want) {
			t.Errorf("缺少 %q：\n%s", want, text)
		}
	}
}

func TestSessionStartResolvesSymlinkedCwd(t *testing.T) {
	saved := ProbeCommand
	t.Cleanup(func() { ProbeCommand = saved })
	ProbeCommand = `printf '%s\n' 15.5 24F74 arm64`

	addr, seen := startRecordingServer(t)
	tmp := t.TempDir()
	realDir := filepath.Join(tmp, "real-project")
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

	text := sessionStart(t, ClientConfig{Addr: addr, Token: testToken}, linkDir)
	if !strings.Contains(text, "macOS 15.5") {
		t.Fatalf("探测失败：\n%s", text)
	}
	if got := waitRecordedCwd(t, seen); got != wantReal {
		t.Fatalf("服务收到的工作目录 %q，期望 %q", got, wantReal)
	}
}

func TestSessionStartWarnsAboutMissingCwd(t *testing.T) {
	_, addr := startServer(t)
	text := sessionStart(t, ClientConfig{Addr: addr, Token: testToken}, "/no/such/dir")
	if !strings.HasPrefix(text, "警告：") || !strings.Contains(text, "命令没有执行") || !strings.Contains(text, "重新启动") {
		t.Fatal(text)
	}
}

func TestSessionStartWarnsWhenServiceIsDown(t *testing.T) {
	saved := ProbeTimeout
	t.Cleanup(func() { ProbeTimeout = saved })
	ProbeTimeout = 2 * time.Second
	_, addr := startServer(t)
	text := sessionStart(t, ClientConfig{Addr: addr, Token: "wrong"}, t.TempDir())
	if !strings.HasPrefix(text, "警告：") {
		t.Fatal(text)
	}
	// 没有服务在听：连接被拒绝。
	text = sessionStart(t, ClientConfig{Addr: "127.0.0.1:1", Token: testToken}, t.TempDir())
	if !strings.Contains(text, "服务不可达") || !strings.Contains(text, "不会改在容器里执行") {
		t.Fatal(text)
	}
}

func TestSessionStartWithoutConfig(t *testing.T) {
	t.Setenv(ConfigEnv, filepath.Join(t.TempDir(), "missing.json"))
	var out bytes.Buffer
	SessionStartMain(strings.NewReader(""), &out)
	if !strings.Contains(out.String(), "配置不可用") {
		t.Fatal(out.String())
	}
}
