package remoteshell

import (
	"bytes"
	"reflect"
	"testing"
)

func TestParseArgs(t *testing.T) {
	cases := []struct {
		args    []string
		want    Invocation
		invalid bool
	}{
		{[]string{"-c", "echo hi"}, Invocation{Command: "echo hi", Args: []string{}}, false},
		{[]string{"-c", "-l", "script"}, Invocation{Command: "script", Args: []string{}}, false},
		{[]string{"-lc", "echo", "name", "a"}, Invocation{Command: "echo", Args: []string{"name", "a"}}, false},
		{[]string{"-l", "-c", "--", "-x"}, Invocation{Command: "-x", Args: []string{}}, false},
		{[]string{"script.sh"}, Invocation{}, true},
		{[]string{"-c"}, Invocation{}, true},
		{nil, Invocation{}, true},
	}
	for _, c := range cases {
		got, err := ParseArgs(c.args)
		if (err != nil) != c.invalid {
			t.Fatalf("%q：错误 %v", c.args, err)
		}
		if !c.invalid && !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%q：得到 %#v", c.args, got)
		}
	}
}

func TestRewriteCwd(t *testing.T) {
	prefix := `source /x/snap.sh 2>/dev/null || true && eval 'ls' && `
	cases := []struct {
		command, path string
		ok            bool
	}{
		{prefix + "pwd -P >| /tmp/claude-b812-cwd", "/tmp/claude-b812-cwd", true},
		{prefix + "pwd -P >| '/tmp/a b/claude-1-cwd'\n", "/tmp/a b/claude-1-cwd", true},
		{prefix + `pwd -P >| "/tmp/x\"y-cwd"`, `/tmp/x"y-cwd`, true},
		// 模型自己写的同样字符串在中间，不是结尾：不改。
		{"eval 'pwd -P >| /tmp/claude-1-cwd; ls'", "", false},
		{prefix + "pwd -P >| $HOME/x-cwd", "", false},
		{"echo hi", "", false},
	}
	for _, c := range cases {
		rewritten, path, ok := RewriteCwd(c.command)
		if ok != c.ok || path != c.path {
			t.Fatalf("%q：得到 %q %v", c.command, path, ok)
		}
		if ok && !bytes.HasSuffix(bytes.TrimSpace([]byte(rewritten)), []byte(`pwd -P >| "$`+CwdFileVar+`"`)) {
			t.Fatalf("改写结果 %q", rewritten)
		}
		if !ok && rewritten != c.command {
			t.Fatalf("不认识的格式被改动了：%q", rewritten)
		}
	}
}

func TestSnapshotFile(t *testing.T) {
	script := "SNAPSHOT_FILE='/home/dev/.claude/shell-snapshots/snapshot-zsh-1-ab.sh'\n" +
		"      source \"/home/dev/.zshrc\" < /dev/null\n" +
		"      echo \"# Snapshot file\" >| \"$SNAPSHOT_FILE\"\n"
	path, ok := SnapshotFile(script)
	if !ok || path != "/home/dev/.claude/shell-snapshots/snapshot-zsh-1-ab.sh" {
		t.Fatalf("得到 %q %v", path, ok)
	}
	if _, ok := SnapshotFile(`echo "$SNAPSHOT_FILE"`); ok {
		t.Fatal("只引用变量不是快照调用")
	}
	if _, ok := SnapshotFile(`eval 'echo hi' && pwd -P >| /tmp/claude-1-cwd`); ok {
		t.Fatal("普通命令不是快照调用")
	}
}

func TestFilterEnv(t *testing.T) {
	got := FilterEnv([]string{"PATH=/usr/bin", "CLAUDECODE=1", "HOME=/home/dev", "CLAUDE_CODE_SESSION_ID=abc",
		"CLAUDE_CODE_MESSAGING_SOCKET=/tmp/s", "CC_REMOTE_ENV_FOO=bar", "CC_REMOTE_ENV_CLAUDECODE=2", "CC_REMOTE_ENV_=x"})
	want := map[string]string{"CLAUDECODE": "2", "CLAUDE_CODE_SESSION_ID": "abc", "FOO": "bar"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("得到 %v", got)
	}
}

func TestFrames(t *testing.T) {
	var buffer bytes.Buffer
	writer := NewFrameWriter(&buffer)
	big := bytes.Repeat([]byte("x"), ChunkSize*2+5)
	if err := writer.WriteChunks(FrameStdout, big); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteJSON(FrameExit, Exit{Code: 3, Cwd: []byte{0xff, '/'}}); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for range 3 {
		kind, payload, err := ReadFrame(&buffer)
		if err != nil || kind != FrameStdout {
			t.Fatalf("%c %v", kind, err)
		}
		got = append(got, payload...)
	}
	if !bytes.Equal(got, big) {
		t.Fatal("输出不一致")
	}
	if kind, _, err := ReadFrame(&buffer); err != nil || kind != FrameExit {
		t.Fatalf("%c %v", kind, err)
	}
	if err := writer.Write(FrameStdout, make([]byte, MaxFrame+1)); err == nil {
		t.Fatal("超长帧应该报错")
	}
	buffer.Reset()
	buffer.Write([]byte{FrameStdout, 0xff, 0xff, 0xff, 0xff})
	if _, _, err := ReadFrame(&buffer); err == nil {
		t.Fatal("声明的长度过大应该报错")
	}
	buffer.Reset()
	buffer.Write([]byte{FrameStdout, 0, 0, 0, 9, 'a'})
	if _, _, err := ReadFrame(&buffer); err == nil {
		t.Fatal("截断的帧应该报错")
	}
}

func TestParseSnapshot(t *testing.T) {
	output := []byte("gitstatus noise\n" + snapshotBegin + "PATH=/opt/homebrew/bin:/usr/bin\x00PWD=/x\x00EMPTY=\x00A=b=c\x00" +
		snapshotEnd + "\nmore noise")
	got, err := parseSnapshot(output)
	want := []string{"PATH=/opt/homebrew/bin:/usr/bin", "EMPTY=", "A=b=c"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("得到 %q %v", got, err)
	}
	if _, err := parseSnapshot([]byte("no markers")); err == nil {
		t.Fatal("没有标记应该报错")
	}
}

func TestMergeEnv(t *testing.T) {
	got := mergeEnv([]string{"A=1", "B=2"}, map[string]string{"B": "3"})
	if !reflect.DeepEqual(got, []string{"A=1", "B=3"}) {
		t.Fatalf("得到 %q", got)
	}
}
