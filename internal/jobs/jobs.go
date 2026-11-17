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
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zeke-chin/cc-runtime/internal/config"
	"github.com/zeke-chin/cc-runtime/internal/dedupe"
	"github.com/zeke-chin/cc-runtime/internal/gitstore"
	"github.com/zeke-chin/cc-runtime/internal/mutagen"
	"github.com/zeke-chin/cc-runtime/internal/util"
)

const (
	LabelPrefix = "io.cc-runtime."
	LaunchdPath = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
)

type Calendar struct{ Hour, Minute int }

type Job struct {
	Name        string
	Description string
	Run         func(cfg config.Config) (int, any, error)
	Calendar    *Calendar // launchd StartCalendarInterval
	Interval    int       // 两次运行之间的秒数
}

func (j Job) ScheduleText() string {
	if j.Calendar != nil {
		return fmt.Sprintf("每天 %02d:%02d", j.Calendar.Hour, j.Calendar.Minute)
	}
	if j.Interval >= 60 {
		return fmt.Sprintf("每 %d 分钟", j.Interval/60)
	}
	return fmt.Sprintf("每 %d 秒", j.Interval)
}

// sessionState 可在测试中替换。
var sessionState = func() (mutagen.SessionState, error) { return mutagen.Query(config.Session) }

func runDedupe(cfg config.Config) (int, any, error) {
	state, err := sessionState()
	if err != nil {
		return 1, nil, err
	}
	if !state.OK() {
		// 两侧可能本来就不一致时，不碰副本。
		status := state.Status
		if status == "" {
			status = "未运行"
		}
		return 2, map[string]any{"skipped": fmt.Sprintf("同步会话状态不正常（%s，冲突 %d）", status, state.Conflicts)}, nil
	}
	var samples []string
	stats, err := dedupe.Run(cfg, dedupe.RunOptions{MinSize: dedupe.MiB, QuietHours: 24, Log: func(m string) {
		if len(samples) < 5 {
			samples = append(samples, m)
		}
	}})
	var unsupported *dedupe.Unsupported
	if errors.As(err, &unsupported) {
		return 3, map[string]any{"error": unsupported.Msg}, nil
	}
	if err != nil {
		return 1, nil, err
	}
	stats.ErrorSamples = samples
	if stats.Errors > 0 {
		return 1, stats, nil
	}
	return 0, stats, nil
}

func runExternalize(cfg config.Config) (int, any, error) {
	layout := gitstore.FromConfig(cfg)
	if err := layout.Check(); err != nil {
		return 1, nil, err
	}
	results := gitstore.ExternalizeAll(layout, false)
	moved, repaired, skipped := []string{}, []string{}, 0
	for _, r := range results {
		switch {
		case r.Changed && r.Repair:
			repaired = append(repaired, r.Rel)
		case r.Changed:
			moved = append(moved, r.Rel)
		default:
			skipped++
		}
	}
	return 0, map[string]any{"externalized": moved, "repaired": repaired, "skipped": skipped}, nil
}

// Jobs 是任务登记表，按显示顺序排列。
var Jobs = []Job{
	{Name: "dedupe", Description: "在两棵目录树之间共享相同文件（文件系统克隆）",
		Run: runDedupe, Calendar: &Calendar{Hour: 4, Minute: 0}},
	{Name: "externalize", Description: "把新仓库的 git 数据移到共享目录，修正只在一侧有效的 worktree/子模块路径",
		Run: runExternalize, Interval: 300},
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
		{"EnvironmentVariables", []KV{{"CC_RUNTIME_ROOT", cfg.Root}, {"HOME", home}, {"PATH", LaunchdPath}}},
		{"StandardOutPath", logPath},
		{"StandardErrorPath", logPath},
		{"ProcessType", "Background"},
		{"LowPriorityIO", true},
		{"Nice", 10},
	}
	if job.Calendar != nil {
		entries = append(entries, KV{"StartCalendarInterval", []KV{{"Hour", job.Calendar.Hour}, {"Minute", job.Calendar.Minute}}})
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
