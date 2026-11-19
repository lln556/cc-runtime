// Package jobs 是定时任务：一份登记表、一个执行入口，由 launchd 负责调度。
//
// 每个任务都经 `cc-runtime job run NAME` 运行：先拿该任务的锁，把结果写到
// <runtime>/state/jobs/NAME.json（status 显示它），并追加到 <runtime>/logs/NAME.log。
// `schedule install` 为每个任务写一个 launchd agent，直接调用安装好的二进制。
package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "time/tzdata" // 时区数据打进二进制，不依赖系统的 zoneinfo

	"golang.org/x/sys/unix"

	"github.com/zeke-chin/cc-runtime/internal/config"
	"github.com/zeke-chin/cc-runtime/internal/util"
)

const (
	LabelPrefix = "io.cc-runtime."
	LaunchdPath = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
)

// Calendar 是每天的运行时刻。Zone 是时刻所在的时区（IANA 名称，空表示 Mac 的本地时区）。
// launchd 只按 Mac 的本地时区触发，所以安装时把时刻换算成 Mac 本地时间写进 plist；
// Mac 的时区或夏令时变了，换算结果会变，plist 就和配置不一致（见 Stale）。
// Jitter 大于 0 时实际运行时刻在目标时刻前后 Jitter 以内随机：launchd 提前一分钟启动，
// 任务先等到随机时刻再运行。
type Calendar struct {
	Hour, Minute int
	Zone         string
	Jitter       time.Duration // 不超过 1 分钟
}

// 可在测试中替换。
var (
	now       = time.Now
	localZone = func() *time.Location { return time.Local }
)

// macTime 是目标时刻（下一次出现时）换算成的 Mac 本地时间。
func (c Calendar) macTime() (hour, minute int, err error) {
	if c.Zone == "" {
		return c.Hour, c.Minute, nil
	}
	zone, err := time.LoadLocation(c.Zone)
	if err != nil {
		return 0, 0, util.Errorf("未知时区 %q", c.Zone)
	}
	current := now().In(zone)
	next := time.Date(current.Year(), current.Month(), current.Day(), c.Hour, c.Minute, 0, 0, zone)
	if !next.After(current) {
		next = time.Date(current.Year(), current.Month(), current.Day()+1, c.Hour, c.Minute, 0, 0, zone)
	}
	local := next.In(localZone())
	return local.Hour(), local.Minute(), nil
}

// launchdTime 是写进 launchd 的启动时刻（Mac 本地时间）：有抖动时提前一分钟。
func (c Calendar) launchdTime() (hour, minute int, err error) {
	hour, minute, err = c.macTime()
	if err != nil {
		return 0, 0, err
	}
	total := hour*60 + minute
	if c.Jitter > 0 {
		total = (total + 24*60 - 1) % (24 * 60)
	}
	return total / 60, total % 60, nil
}

// delay 是 launchd 启动后要等的时间：一分钟减去 [-Jitter, +Jitter] 内的随机偏移。
func (c Calendar) delay(random func(n int64) int64) time.Duration {
	if c.Jitter <= 0 {
		return 0
	}
	offset := time.Duration(random(int64(2*c.Jitter)+1)) - c.Jitter
	return time.Minute + offset
}

// ScheduledEnv 由 launchd 设置，表示这次运行是定时触发的（手动 job run 不等待抖动）。
const ScheduledEnv = "CC_RUNTIME_SCHEDULED"

var sleep = time.Sleep

type Job struct {
	Name        string
	Description string
	Run         func(cfg config.Config) (int, any, error)
	Calendar    *Calendar // launchd StartCalendarInterval
	Interval    int       // 两次运行之间的秒数
}

// settingKey 是任务在 .env 里的配置名：wake-claude 的 TIME 是 WAKE_CLAUDE_TIME。
func settingKey(job, setting string) string {
	return strings.ToUpper(strings.ReplaceAll(job, "-", "_")) + "_" + setting
}

func setting(cfg config.Config, job, name string) (string, bool) {
	value, ok := cfg.Env[settingKey(job, name)]
	value = strings.TrimSpace(value)
	return value, ok && value != ""
}

// CalendarFor 是任务实际的运行时刻：登记表里的默认值，可在 .env 里用
// <任务>_TIME（HH:MM）、<任务>_TZ（IANA 时区名，例如 Asia/Shanghai）和
// <任务>_JITTER（秒，0–60）覆盖。没有固定时刻的任务返回 nil。
func (j Job) CalendarFor(cfg config.Config) (*Calendar, error) {
	if j.Calendar == nil {
		return nil, nil
	}
	c := *j.Calendar
	if value, ok := setting(cfg, j.Name, "TIME"); ok {
		parsed, err := time.Parse("15:04", value)
		if err != nil {
			return nil, util.Errorf("%s=%q 不是 HH:MM 格式的时刻", settingKey(j.Name, "TIME"), value)
		}
		c.Hour, c.Minute = parsed.Hour(), parsed.Minute()
	}
	if value, ok := setting(cfg, j.Name, "TZ"); ok {
		if _, err := time.LoadLocation(value); err != nil || value == "Local" {
			return nil, util.Errorf("%s=%q 不是 IANA 时区名（例如 Asia/Shanghai）", settingKey(j.Name, "TZ"), value)
		}
		c.Zone = value
	}
	if value, ok := setting(cfg, j.Name, "JITTER"); ok {
		seconds, err := strconv.Atoi(value)
		if err != nil || seconds < 0 || seconds > 60 {
			return nil, util.Errorf("%s=%q 应为 0 到 60 之间的秒数", settingKey(j.Name, "JITTER"), value)
		}
		c.Jitter = time.Duration(seconds) * time.Second
	}
	return &c, nil
}

func (j Job) ScheduleText(cfg config.Config) string {
	c, err := j.CalendarFor(cfg)
	if err != nil {
		return "配置有误：" + err.Error()
	}
	if c != nil {
		text := fmt.Sprintf("每天 %02d:%02d", c.Hour, c.Minute)
		if c.Zone != "" {
			text += " " + c.Zone
		}
		var notes []string
		if c.Jitter > 0 {
			notes = append(notes, fmt.Sprintf("±%d 秒", int(c.Jitter.Seconds())))
		}
		if hour, minute, err := c.macTime(); err != nil {
			notes = append(notes, err.Error())
		} else if hour != c.Hour || minute != c.Minute {
			notes = append(notes, fmt.Sprintf("Mac 本地 %02d:%02d", hour, minute))
		}
		if len(notes) > 0 {
			text += "（" + strings.Join(notes, "，") + "）"
		}
		return text
	}
	if j.Interval >= 60 {
		return fmt.Sprintf("每 %d 分钟", j.Interval/60)
	}
	return fmt.Sprintf("每 %d 秒", j.Interval)
}

// Jobs 是任务登记表，按显示顺序排列。
var Jobs = []Job{
	{
		Name:        "wake-claude",
		Description: "唤醒开发机里的 Claude：发一条简短的请求",
		Run:         runWakeClaude,
		Calendar:    &Calendar{Hour: 8, Minute: 30, Zone: "Asia/Shanghai", Jitter: 10 * time.Second},
	},
}

// wake-claude 的默认值；运行时可在 .env 里用 WAKE_CLAUDE_MACHINES（逗号分隔的
// Compose 服务名）和 WAKE_CLAUDE_PROMPT 覆盖。
var (
	wakeMachines = []string{"dev-pennsylvania"}
	wakePrompt   = "只回复 ok"
)

func wakeSettings(cfg config.Config) (machines []string, prompt string) {
	machines, prompt = wakeMachines, wakePrompt
	if value, ok := setting(cfg, "wake-claude", "MACHINES"); ok {
		machines = nil
		for _, item := range strings.Split(value, ",") {
			if item = strings.TrimSpace(item); item != "" {
				machines = append(machines, item)
			}
		}
	}
	if value, ok := setting(cfg, "wake-claude", "PROMPT"); ok {
		prompt = value
	}
	return machines, prompt
}

// Exec 在开发机里以 dev 用户运行命令，由 cli 设为 deploy.Exec
// （deploy 经 remoteshell 依赖本包，这里不能直接引用它）。
var Exec func(cfg config.Config, service string, args ...string) (util.Result, error)

// claudeExec 在开发机里运行 claude；测试中替换。
var claudeExec = func(cfg config.Config, machine, prompt string) (util.Result, error) {
	if Exec == nil {
		return util.Result{}, util.Errorf("没有设置在开发机里运行命令的方法")
	}
	// 在工作区里启动：Bash 命令经 remote-shell 在 Mac 上执行，家目录在 Mac 上不存在。
	script := `cd "${WORKSPACE_DIR:-$HOME}" && exec timeout 300 claude -p "$1" --output-format text`
	return Exec(cfg, machine, "sh", "-c", script, "sh", prompt)
}

func runWakeClaude(cfg config.Config) (int, any, error) {
	code := 0
	summary := map[string]any{}
	machines, prompt := wakeSettings(cfg)
	for _, machine := range machines {
		result, err := claudeExec(cfg, machine, prompt)
		reply := strings.TrimSpace(result.Stdout)
		if err != nil || result.Code != 0 {
			code = 1
			detail := strings.TrimSpace(result.Stdout + "\n" + result.Stderr)
			if err != nil && detail == "" {
				detail = err.Error()
			}
			summary[machine] = map[string]any{"exit": result.Code, "error": tail(detail, 500)}
			continue
		}
		summary[machine] = map[string]any{"reply": tail(reply, 200)}
	}
	return code, summary, nil
}

func tail(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return "…" + string(runes[len(runes)-limit:])
}

func Names() []string {
	names := make([]string, len(Jobs))
	for i, job := range Jobs {
		names[i] = job.Name
	}
	return names
}

func Get(name string) (Job, error) {
	for _, job := range Jobs {
		if job.Name == name {
			return job, nil
		}
	}
	return Job{}, util.Errorf("未知任务 %q；可用：%s", name, strings.Join(Names(), ", "))
}

func recordPath(cfg config.Config, name string) string {
	return filepath.Join(cfg.StateDir(), "jobs", name+".json")
}

// Record 是一次运行的结果。字段顺序就是 JSON 里的顺序。
type Record struct {
	Job     string  `json:"job"`
	Started string  `json:"started"`
	Seconds float64 `json:"seconds"`
	Exit    int     `json:"exit"`
	Summary any     `json:"summary"`
}

// LastRun 读取上次运行的记录；没有时返回 nil。
func LastRun(cfg config.Config, name string) map[string]any {
	data, err := os.ReadFile(recordPath(cfg, name))
	if err != nil {
		return nil
	}
	var record map[string]any
	if json.Unmarshal(data, &record) != nil {
		return nil
	}
	return record
}

func marshal(value any) []byte {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(value)
	return []byte(b.String())
}

func RunJob(cfg config.Config, name string) (Record, error) {
	job, err := Get(name)
	if err != nil {
		return Record{}, err
	}
	if os.Getenv(ScheduledEnv) == "1" {
		if c, err := job.CalendarFor(cfg); err == nil && c != nil {
			sleep(c.delay(rand.Int64N))
		}
	}
	if err := os.MkdirAll(filepath.Join(cfg.StateDir(), "jobs"), 0o755); err != nil {
		return Record{}, err
	}
	if err := os.MkdirAll(cfg.LogDir(), 0o755); err != nil {
		return Record{}, err
	}
	lock, err := os.OpenFile(filepath.Join(cfg.StateDir(), "jobs", name+".lock"), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return Record{}, err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return Record{Job: name, Exit: 0, Summary: map[string]any{"skipped": "已在运行"}}, nil
	}
	started := time.Now()
	code, summary := runSafely(job, cfg)
	record := Record{Job: name, Started: started.Format("2006-01-02T15:04:05"),
		Seconds: math.Round(time.Since(started).Seconds()*10) / 10, Exit: code, Summary: summary}
	line := marshal(record)
	temporary := strings.TrimSuffix(recordPath(cfg, name), ".json") + ".tmp"
	if err := os.WriteFile(temporary, line, 0o644); err != nil {
		return record, err
	}
	if err := os.Rename(temporary, recordPath(cfg, name)); err != nil {
		return record, err
	}
	log, err := os.OpenFile(filepath.Join(cfg.LogDir(), name+".log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return record, err
	}
	defer log.Close()
	_, err = log.Write(line)
	return record, err
}

// runSafely 运行任务；出错或崩溃都记为失败，定时任务的失败才看得见。
func runSafely(job Job, cfg config.Config) (code int, summary any) {
	defer func() {
		if r := recover(); r != nil {
			code, summary = 1, map[string]any{"error": fmt.Sprintf("panic: %v", r)}
		}
	}()
	code, summary, err := job.Run(cfg)
	if err != nil {
		return 1, map[string]any{"error": err.Error()}
	}
	return code, summary
}

// ---- launchd ------------------------------------------------------------------

func Label(name string) string { return LabelPrefix + name }

func PlistPath(name string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", Label(name)+".plist")
}

// executable 是安装好的 cc-runtime 的路径；`go run` 的临时文件不能写进 launchd。
var executable = func() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	if strings.Contains(path, "go-build") || strings.HasPrefix(path, os.TempDir()) {
		return "", util.Errorf("当前运行的是临时编译的程序（%s）；请先运行 scripts/install.sh 安装，再用安装好的 cc-runtime 执行 schedule install", path)
	}
	return path, nil
}

// Plist 生成任务的 launchd 定义（XML 文本）。
func Plist(cfg config.Config, name string) (string, error) {
	job, err := Get(name)
	if err != nil {
		return "", err
	}
	binary, err := executable()
	if err != nil {
		return "", err
	}
	home, _ := os.UserHomeDir()
	logPath := filepath.Join(cfg.LogDir(), name+".launchd.log")
	entries := []KV{
		{"Label", Label(name)},
		{"ProgramArguments", []string{binary, "job", "run", name}},
		{"EnvironmentVariables", []KV{{"CC_RUNTIME_ROOT", cfg.Root}, {ScheduledEnv, "1"}, {"HOME", home}, {"PATH", LaunchdPath}}},
		{"StandardOutPath", logPath},
		{"StandardErrorPath", logPath},
		{"ProcessType", "Background"},
		{"LowPriorityIO", true},
		{"Nice", 10},
	}
	calendar, err := job.CalendarFor(cfg)
	if err != nil {
		return "", err
	}
	if calendar != nil {
		hour, minute, err := calendar.launchdTime()
		if err != nil {
			return "", err
		}
		entries = append(entries, KV{"StartCalendarInterval", []KV{{"Hour", hour}, {"Minute", minute}}})
	}
	if job.Interval > 0 {
		entries = append(entries, KV{"StartInterval", job.Interval})
	}
	return RenderPlist(entries), nil
}

// RenderPlist 把键值表写成 launchd 的 plist（XML 文本）。
func RenderPlist(entries []KV) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
`)
	writeValue(&b, entries, "")
	b.WriteString("</plist>\n")
	return b.String()
}

// Executable 是安装好的 cc-runtime 的路径（不能是 go run 的临时文件）。
func Executable() (string, error) { return executable() }

// KV 是 plist 字典里的一项；值可以是 string、int、bool、[]string 或 []KV。
type KV struct {
	Key   string
	Value any
}

func writeValue(b *strings.Builder, value any, indent string) {
	switch v := value.(type) {
	case []KV:
		sort.SliceStable(v, func(i, j int) bool { return v[i].Key < v[j].Key })
		b.WriteString(indent + "<dict>\n")
		for _, item := range v {
			b.WriteString(indent + "\t<key>" + html.EscapeString(item.Key) + "</key>\n")
			writeValue(b, item.Value, indent+"\t")
		}
		b.WriteString(indent + "</dict>\n")
	case []string:
		b.WriteString(indent + "<array>\n")
		for _, item := range v {
			writeValue(b, item, indent+"\t")
		}
		b.WriteString(indent + "</array>\n")
	case string:
		b.WriteString(indent + "<string>" + html.EscapeString(v) + "</string>\n")
	case int:
		b.WriteString(fmt.Sprintf("%s<integer>%d</integer>\n", indent, v))
	case bool:
		if v {
			b.WriteString(indent + "<true/>\n")
		} else {
			b.WriteString(indent + "<false/>\n")
		}
	}
}

func requireMacOS() error {
	if runtime.GOOS != "darwin" {
		return util.Errorf("定时任务依赖 launchd，只支持 macOS；在 Linux 上请用 systemd timer 运行 `cc-runtime job run NAME`")
	}
	return nil
}

func Domain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

func Install(cfg config.Config, names []string) error {
	if err := requireMacOS(); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.LogDir(), 0o755); err != nil {
		return err
	}
	for _, name := range names {
		text, err := Plist(cfg, name)
		if err != nil {
			return err
		}
		path := PlistPath(name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			return err
		}
		util.Run([]string{"launchctl", "bootout", Domain(), path}, util.Opts{Capture: true, AllowFail: true})
		if _, err := util.Run([]string{"launchctl", "bootstrap", Domain(), path}, util.Opts{Capture: true}); err != nil {
			return err
		}
	}
	return nil
}

func Uninstall(names []string) error {
	if err := requireMacOS(); err != nil {
		return err
	}
	for _, name := range names {
		path := PlistPath(name)
		util.Run([]string{"launchctl", "bootout", Domain(), path}, util.Opts{Capture: true, AllowFail: true})
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Stale 表示已安装的 plist 和按当前配置生成的不一致（时刻、时区、Mac 的时区或夏令时、
// 二进制路径变了），需要重新运行 schedule install。没有安装时返回 false。
func Stale(cfg config.Config, name string) bool {
	installed, err := os.ReadFile(PlistPath(name))
	if err != nil {
		return false
	}
	expected, err := Plist(cfg, name)
	return err != nil || string(installed) != expected
}

// Scheduled 返回（agent 文件存在，agent 已加载到 launchd）。
func Scheduled(name string) (present, loaded bool) {
	if runtime.GOOS != "darwin" {
		return false, false
	}
	result, err := util.Run([]string{"launchctl", "print", Domain() + "/" + Label(name)},
		util.Opts{Capture: true, AllowFail: true})
	loaded = err == nil && result.Code == 0
	return util.Lexists(PlistPath(name)), loaded
}
