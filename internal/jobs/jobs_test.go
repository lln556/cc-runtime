package jobs

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeke-chin/cc-runtime/internal/config"
	"github.com/zeke-chin/cc-runtime/internal/testutil"
)

func fake(t *testing.T, run func(config.Config) (int, any, error)) {
	saved := Jobs
	t.Cleanup(func() { Jobs = saved })
	Jobs = []Job{{Name: "fake", Description: "test job", Run: run, Interval: 60}}
}

func TestRunRecordsOutcomeAndAppendsLog(t *testing.T) {
	cfg := testutil.MakeConfig(t, t.TempDir(), "")
	fake(t, func(config.Config) (int, any, error) { return 0, map[string]any{"n": 1}, nil })
	record, err := RunJob(cfg, "fake")
	if err != nil {
		t.Fatal(err)
	}
	RunJob(cfg, "fake")
	if record.Exit != 0 {
		t.Fatal(record)
	}
	if summary := LastRun(cfg, "fake")["summary"].(map[string]any); summary["n"] != 1.0 {
		t.Fatal(summary)
	}
	data, _ := os.ReadFile(filepath.Join(cfg.LogDir(), "fake.log"))
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var first map[string]any
	if len(lines) != 2 || json.Unmarshal([]byte(lines[0]), &first) != nil || first["job"] != "fake" {
		t.Fatal(lines)
	}
}

func TestACrashingJobIsRecordedAsFailed(t *testing.T) {
	cfg := testutil.MakeConfig(t, t.TempDir(), "")
	fake(t, func(config.Config) (int, any, error) { return 0, nil, errors.New("boom") })
	record, _ := RunJob(cfg, "fake")
	if record.Exit != 1 || !strings.Contains(record.Summary.(map[string]any)["error"].(string), "boom") {
		t.Fatal(record)
	}
	fake(t, func(config.Config) (int, any, error) { panic("kaboom") })
	record, _ = RunJob(cfg, "fake")
	if record.Exit != 1 || !strings.Contains(record.Summary.(map[string]any)["error"].(string), "kaboom") {
		t.Fatal(record)
	}
}

func TestLaunchdDefinition(t *testing.T) {
	cfg := testutil.MakeConfig(t, t.TempDir(), "")
	saved := executable
	defer func() { executable = saved }()
	executable = func() (string, error) { return "/Users/x/.local/bin/cc-runtime", nil }
	saved2 := Jobs
	t.Cleanup(func() { Jobs = saved2 })
	Jobs = []Job{
		{Name: "daily", Description: "每天", Calendar: &Calendar{Hour: 4, Minute: 0}},
		{Name: "frequent", Description: "频繁", Interval: 300},
	}
	daily, err := Plist(cfg, "daily")
	if err != nil {
		t.Fatal(err)
	}
	frequent, _ := Plist(cfg, "frequent")
	for _, fragment := range []string{
		"<key>Label</key>\n\t<string>io.cc-runtime.daily</string>",
		"<key>StartCalendarInterval</key>\n\t<dict>\n\t\t<key>Hour</key>\n\t\t<integer>4</integer>\n\t\t<key>Minute</key>\n\t\t<integer>0</integer>",
		"<array>\n\t\t<string>/Users/x/.local/bin/cc-runtime</string>\n\t\t<string>job</string>\n\t\t<string>run</string>\n\t\t<string>daily</string>\n\t</array>",
		"<key>CC_RUNTIME_ROOT</key>\n\t\t<string>" + cfg.Root + "</string>",
		"/usr/local/bin",
		"<key>LowPriorityIO</key>\n\t<true/>",
	} {
		if !strings.Contains(daily, fragment) {
			t.Errorf("缺少 %q：\n%s", fragment, daily)
		}
	}
	if !strings.Contains(frequent, "<key>StartInterval</key>\n\t<integer>300</integer>") {
		t.Error(frequent)
	}
	if strings.Contains(daily, "uv") {
		t.Error("launchd 不应再经过 uv")
	}
	first, _ := Get("daily")
	second, _ := Get("frequent")
	if first.ScheduleText(cfg) != "每天 04:00" || second.ScheduleText(cfg) != "每 5 分钟" {
		t.Fatal(first.ScheduleText(cfg), second.ScheduleText(cfg))
	}
}

func TestTemporaryBinaryIsRefused(t *testing.T) {
	// 测试本身就是 go test 临时编译的程序，正好用来验证。
	if _, err := executable(); err == nil || !strings.Contains(err.Error(), "install.sh") {
		t.Fatal(err)
	}
}
