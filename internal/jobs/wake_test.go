package jobs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeke-chin/cc-runtime/internal/config"
	"github.com/zeke-chin/cc-runtime/internal/testutil"
	"github.com/zeke-chin/cc-runtime/internal/util"
)

func TestJitteredCalendarStartsOneMinuteEarly(t *testing.T) {
	cases := []struct {
		c            Calendar
		hour, minute int
	}{
		{Calendar{Hour: 8, Minute: 30}, 8, 30},
		{Calendar{Hour: 8, Minute: 30, Jitter: 10 * time.Second}, 8, 29},
		{Calendar{Hour: 9, Minute: 0, Jitter: 10 * time.Second}, 8, 59},
		{Calendar{Hour: 0, Minute: 0, Jitter: 10 * time.Second}, 23, 59},
	}
	for _, tc := range cases {
		if hour, minute, _ := tc.c.launchdTime(); hour != tc.hour || minute != tc.minute {
			t.Errorf("%+v: %02d:%02d", tc.c, hour, minute)
		}
	}
}

func TestJitterDelayStaysWithinWindow(t *testing.T) {
	c := Calendar{Hour: 8, Minute: 30, Jitter: 10 * time.Second}
	lowest := c.delay(func(int64) int64 { return 0 })
	highest := c.delay(func(n int64) int64 { return n - 1 })
	if lowest != 50*time.Second || highest != 70*time.Second {
		t.Fatal(lowest, highest)
	}
	if d := (Calendar{Hour: 8}).delay(func(int64) int64 { return 5 }); d != 0 {
		t.Fatal(d)
	}
}

func TestScheduledRunWaitsButManualRunDoesNot(t *testing.T) {
	cfg := testutil.MakeConfig(t, t.TempDir(), "")
	saved, savedSleep := Jobs, sleep
	t.Cleanup(func() { Jobs, sleep = saved, savedSleep })
	Jobs = []Job{{Name: "daily", Run: func(config.Config) (int, any, error) { return 0, nil, nil },
		Calendar: &Calendar{Hour: 8, Minute: 30, Jitter: 10 * time.Second}}}
	var waited []time.Duration
	sleep = func(d time.Duration) { waited = append(waited, d) }

	t.Setenv(ScheduledEnv, "")
	RunJob(cfg, "daily")
	if len(waited) != 0 {
		t.Fatal(waited)
	}
	t.Setenv(ScheduledEnv, "1")
	RunJob(cfg, "daily")
	if len(waited) != 1 || waited[0] < 50*time.Second || waited[0] > 70*time.Second {
		t.Fatal(waited)
	}
}

func TestWakeClaudeIsRegisteredAtEightThirty(t *testing.T) {
	job, err := Get("wake-claude")
	if err != nil {
		t.Fatal(err)
	}
	useZones(t, "Asia/Shanghai", time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC))
	if got := job.ScheduleText(config.Config{}); got != "每天 08:30 Asia/Shanghai（±10 秒）" {
		t.Fatal(got)
	}
	cfg := testutil.MakeConfig(t, t.TempDir(), "")
	savedExe := executable
	t.Cleanup(func() { executable = savedExe })
	executable = func() (string, error) { return "/usr/local/bin/cc-runtime", nil }
	text, err := Plist(cfg, "wake-claude")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<key>Hour</key>\n\t\t<integer>8</integer>",
		"<key>Minute</key>\n\t\t<integer>29</integer>",
		"<key>" + ScheduledEnv + "</key>\n\t\t<string>1</string>",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("缺少 %q：\n%s", want, text)
		}
	}
}

func TestWakeClaudeSummarizesEachMachine(t *testing.T) {
	savedExec, savedMachines := claudeExec, wakeMachines
	t.Cleanup(func() { claudeExec, wakeMachines = savedExec, savedMachines })
	wakeMachines = []string{"dev-a", "dev-b"}
	var prompts []string
	claudeExec = func(_ config.Config, machine, prompt string) (util.Result, error) {
		prompts = append(prompts, prompt)
		if machine == "dev-b" {
			return util.Result{Stderr: "limit reached", Code: 1}, errors.New("exit 1")
		}
		return util.Result{Stdout: "ok\n"}, nil
	}
	code, summary, err := runWakeClaude(config.Config{})
	if err != nil || code != 1 {
		t.Fatal(code, err)
	}
	s := summary.(map[string]any)
	if s["dev-a"].(map[string]any)["reply"] != "ok" {
		t.Fatal(s)
	}
	if !strings.Contains(s["dev-b"].(map[string]any)["error"].(string), "limit reached") {
		t.Fatal(s)
	}
	if prompts[0] != "只回复 ok" {
		t.Fatal(prompts)
	}
}

func TestSettingsInEnvOverrideDefaults(t *testing.T) {
	job, _ := Get("wake-claude")
	cfg := config.Config{Env: map[string]string{
		"WAKE_CLAUDE_TIME": "07:05", "WAKE_CLAUDE_JITTER": "0",
		"WAKE_CLAUDE_MACHINES": "dev-a, dev-b", "WAKE_CLAUDE_PROMPT": "hi",
	}}
	useZones(t, "Asia/Shanghai", time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC))
	if got := job.ScheduleText(cfg); got != "每天 07:05 Asia/Shanghai" {
		t.Fatal(got)
	}
	machines, prompt := wakeSettings(cfg)
	if strings.Join(machines, "|") != "dev-a|dev-b" || prompt != "hi" {
		t.Fatal(machines, prompt)
	}
	for _, bad := range []map[string]string{{"WAKE_CLAUDE_TIME": "8点半"}, {"WAKE_CLAUDE_JITTER": "90"}, {"WAKE_CLAUDE_TZ": "Mars/Base"}} {
		if _, err := job.CalendarFor(config.Config{Env: bad}); err == nil {
			t.Errorf("%v 应该报错", bad)
		}
	}
}

// useZones 把 Mac 的本地时区和当前时间换成测试用的值。
func useZones(t *testing.T, local string, at time.Time) {
	t.Helper()
	zone, err := time.LoadLocation(local)
	if err != nil {
		t.Fatal(err)
	}
	savedNow, savedLocal := now, localZone
	t.Cleanup(func() { now, localZone = savedNow, savedLocal })
	now = func() time.Time { return at }
	localZone = func() *time.Location { return zone }
}

func TestZoneIsConvertedToMacLocalTime(t *testing.T) {
	c := Calendar{Hour: 8, Minute: 30, Zone: "Asia/Shanghai", Jitter: 10 * time.Second}
	cases := []struct {
		local        string
		at           time.Time
		hour, minute int
	}{
		// Mac 在北京：不用换算，有抖动时提前一分钟。
		{"Asia/Shanghai", time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC), 8, 29},
		// Mac 在纽约：冬令时北京 08:30 是前一天 19:30，夏令时是 20:30。
		{"America/New_York", time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC), 19, 29},
		{"America/New_York", time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC), 20, 29},
		// 半小时时差。
		{"Asia/Kolkata", time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC), 5, 59},
	}
	for _, tc := range cases {
		useZones(t, tc.local, tc.at)
		if hour, minute, err := c.launchdTime(); err != nil || hour != tc.hour || minute != tc.minute {
			t.Errorf("%s %s：%02d:%02d %v", tc.local, tc.at, hour, minute, err)
		}
	}
	useZones(t, "America/New_York", time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC))
	job := Job{Name: "x", Calendar: &c}
	if got := job.ScheduleText(config.Config{}); got != "每天 08:30 Asia/Shanghai（±10 秒，Mac 本地 19:30）" {
		t.Fatal(got)
	}
}

func TestStaleWhenMacZoneChanges(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := testutil.MakeConfig(t, t.TempDir(), "")
	savedExe := executable
	t.Cleanup(func() { executable = savedExe })
	executable = func() (string, error) { return "/usr/local/bin/cc-runtime", nil }
	useZones(t, "Asia/Shanghai", time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC))
	if Stale(cfg, "wake-claude") {
		t.Fatal("没有安装时不算不一致")
	}
	text, _ := Plist(cfg, "wake-claude")
	os.MkdirAll(filepath.Dir(PlistPath("wake-claude")), 0o755)
	os.WriteFile(PlistPath("wake-claude"), []byte(text), 0o644)
	if Stale(cfg, "wake-claude") {
		t.Fatal("刚安装不应不一致")
	}
	useZones(t, "America/New_York", time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC))
	if !Stale(cfg, "wake-claude") {
		t.Fatal("Mac 换了时区应提示重装")
	}
}
