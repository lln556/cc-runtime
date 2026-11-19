package cli

import (
	"reflect"
	"strings"
	"testing"

	"github.com/zeke-chin/cc-runtime/internal/ui"
)

func TestToolVersion(t *testing.T) {
	cases := map[string]string{
		"ugrep 7.8.4 aarch64-apple-macosx +neon/AArch64; -P:pcre2jit": "7.8.4",
		"bfs 4.1.1":                       "4.1.1",
		"ripgrep 14.1.1 (rev 6634fc3b5b)": "14.1.1",
		"":                                "",
		"ugrep":                           "",
	}
	for line, want := range cases {
		if got := toolVersion(line); got != want {
			t.Errorf("toolVersion(%q) = %q，应为 %q", line, got, want)
		}
	}
}

func TestParseClaudeTools(t *testing.T) {
	output := "claude\t/home/dev/.local/bin/claude\t2.1.328 (Claude Code)\nugrep\tugrep 7.8.4 x\nbfs\tbfs 4.1.1\nrg\t\nug\tugrep 7.8.4 x\n"
	got := parseClaudeTools(output)
	if got.Path != "/home/dev/.local/bin/claude" || got.Version != "2.1.328 (Claude Code)" {
		t.Fatalf("%#v", got)
	}
	if want := "2.1.328 (Claude Code) 内置 ugrep 7.8.4、bfs 4.1.1、ripgrep 无、ug 7.8.4"; got.describe() != want {
		t.Errorf("describe = %q", got.describe())
	}
	if missing := parseClaudeTools("claude\t\t\n"); missing.Path != "" {
		t.Errorf("没装时路径应为空：%#v", missing)
	}
}

func TestParseMacTools(t *testing.T) {
	output := "ugrep\t/r/search-tools/ugrep\t/v/2.1.328\tugrep 7.8.4 x\nbfs\t\t\t\nrg\t\t\t\n" +
		"__CC_SPLIT__\n" +
		"grep\tfunction\t/usr/bin/grep\tgrep (BSD grep) 2.6.0\nfind\tcommand\t/usr/bin/find\t\nrg\tcommand\t/opt/homebrew/bin/rg\tripgrep 15.2.0\n"
	tools, names, ok := parseMacTools(output)
	if !ok {
		t.Fatal("没有识别分隔行")
	}
	wantTools := map[string]dirTool{
		"ugrep": {"/r/search-tools/ugrep", "/v/2.1.328", "7.8.4"},
		"bfs":   {},
		"rg":    {},
	}
	if !reflect.DeepEqual(tools, wantTools) {
		t.Errorf("tools = %#v", tools)
	}
	wantNames := map[string]resolved{
		"grep": {"function", "/usr/bin/grep", "(BSD"},
		"find": {"command", "/usr/bin/find", ""},
		"rg":   {"command", "/opt/homebrew/bin/rg", "15.2.0"},
	}
	if !reflect.DeepEqual(names, wantNames) {
		t.Errorf("names = %#v", names)
	}
	if _, _, ok := parseMacTools("ugrep\t\t\t\n"); ok {
		t.Error("没有分隔行时应失败")
	}
}

func TestToolSource(t *testing.T) {
	short := func(path string) string { return strings.Replace(path, "/Users/z", "~", 1) }
	cases := []struct {
		tool dirTool
		want string
	}{
		{dirTool{Path: "/r/ugrep", Target: "/Users/z/.local/share/claude/versions/2.1.328"}, "Claude Code 2.1.328"},
		{dirTool{Path: "/r/bfs", Target: "/opt/homebrew/Cellar/bfs/4.1.7/bin/bfs"}, "/opt/homebrew/Cellar/bfs/4.1.7/bin/bfs"},
		{dirTool{Path: "/r/rg", Target: "/r/rg"}, ""},
		{dirTool{Path: "/r/rg"}, ""},
	}
	for _, c := range cases {
		if got := toolSource(c.tool, short); got != c.want {
			t.Errorf("toolSource(%+v) = %q，应为 %q", c.tool, got, c.want)
		}
	}
}

func TestDescribeEmbedded(t *testing.T) {
	if got := describeEmbedded(nil); got != "未知" {
		t.Errorf("空版本 = %q", got)
	}
	same := map[string]string{"dev-pennsylvania": "7.8.4", "dev-california": "7.8.4"}
	if got := describeEmbedded(same); got != "7.8.4" {
		t.Errorf("版本相同 = %q", got)
	}
	differ := map[string]string{"dev-pennsylvania": "7.8.4", "dev-california": "7.8.3"}
	if got := describeEmbedded(differ); got != "dev-california 7.8.3、dev-pennsylvania 7.8.4" {
		t.Errorf("版本不同 = %q", got)
	}
}

func TestMergeCell(t *testing.T) {
	ui.SetColor(false)
	same := mergeCell(ui.C("ugrep 7.8.4（Claude Code 2.1.328）", ui.Green), ui.C("ug 7.8.4（Claude Code 2.1.328）", ui.Green), "ugrep", "ug")
	if same.Text != "ugrep 7.8.4（Claude Code 2.1.328）" {
		t.Errorf("同一个程序应只留一个：%q", same.Text)
	}
	if got := mergeCell(ui.C("ugrep（目录里的）", ui.Green), ui.C("ug（目录里的）", ui.Green), "ugrep", "ug"); got.Text != "ugrep（目录里的）" {
		t.Errorf("都被接管时应只留一个：%q", got.Text)
	}
	differ := mergeCell(ui.C("/usr/bin/grep", ui.None), ui.C("无", ui.Dim), "ugrep", "ug")
	if differ.Text != "/usr/bin/grep；ug 无" || differ.Style != ui.Dim {
		t.Errorf("不同时应连起来并取第二个的颜色：%q %q", differ.Text, differ.Style)
	}
	missing := mergeCell(ui.C("缺少", ui.Red), ui.C("ug 7.8.4", ui.Green), "ugrep", "ug")
	if missing.Text != "缺少；ug 7.8.4" || missing.Style != ui.Red {
		t.Errorf("第二个正常时保留第一个的颜色：%q %q", missing.Text, missing.Style)
	}
}
