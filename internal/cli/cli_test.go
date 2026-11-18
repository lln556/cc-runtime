package cli

import (
	"slices"
	"strings"
	"testing"
)

func TestOptionsAndPositionalsMayInterleave(t *testing.T) {
	fs := newFlags("schedule")
	all := fs.Bool("all", false, "")
	dryRun := fs.Bool("dry-run", false, "")
	got, err := parse("schedule", fs, []string{"install", "--dry-run", "a/b", "--all", "c"})
	if err != nil || !*all || !*dryRun || !slices.Equal(got, []string{"install", "a/b", "c"}) {
		t.Fatal(got, err)
	}
}

func TestWatchFlagHasOptionalSeconds(t *testing.T) {
	for args, want := range map[string]float64{"--watch": 5, "--watch=2.5": 2.5, "": 0} {
		fs := newFlags("status")
		var w watchFlag
		fs.Var(&w, "watch", "")
		var list []string
		if args != "" {
			list = []string{args}
		}
		if _, err := parse("status", fs, list); err != nil || w.seconds != want {
			t.Errorf("%q：%v %v", args, w.seconds, err)
		}
	}
}

func TestUnknownOptionIsReportedInChinese(t *testing.T) {
	_, err := parse("job", newFlags("job"), []string{"--nope"})
	if err == nil || !strings.Contains(err.Error(), "未知选项") {
		t.Fatal(err)
	}
}

func TestChoose(t *testing.T) {
	if _, err := choose("schedule", "操作", []string{"bogus"}, "status", "create"); err == nil {
		t.Fatal("无效操作应报错")
	}
	if _, err := choose("schedule", "操作", nil, "status"); err == nil {
		t.Fatal("缺少操作应报错")
	}
}
