package cli

import (
	"reflect"
	"testing"
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

func TestParseToolLines(t *testing.T) {
	output := "ugrep\t/opt/homebrew/bin/ugrep\tugrep 7.8.5 x\nbfs\t\t\nrg\t/opt/homebrew/bin/rg\tripgrep 15.1.0\n\n"
	got := parseToolLines(output)
	want := map[string][]string{
		"ugrep": {"/opt/homebrew/bin/ugrep", "ugrep 7.8.5 x"},
		"bfs":   {"", ""},
		"rg":    {"/opt/homebrew/bin/rg", "ripgrep 15.1.0"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseToolLines = %#v，应为 %#v", got, want)
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
