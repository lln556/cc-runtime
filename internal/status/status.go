// Package status 是 `status` 状态面板：同步、定时任务、git 数据、磁盘和容器放在一屏里。
package status

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zeke-chin/cc-runtime/internal/config"
	"github.com/zeke-chin/cc-runtime/internal/gitstore"
	"github.com/zeke-chin/cc-runtime/internal/jobs"
	"github.com/zeke-chin/cc-runtime/internal/mutagen"
	"github.com/zeke-chin/cc-runtime/internal/remoteshell"
	"github.com/zeke-chin/cc-runtime/internal/ui"
	"github.com/zeke-chin/cc-runtime/internal/util"
)

const (
	OK   = ui.Green
	Warn = ui.Yellow
	Bad  = ui.Red
	Dim  = ui.Dim
)

func ago(stamp string) string {
	if stamp == "" {
		return "从未"
	}
	t, err := time.ParseInLocation("2006-01-02T15:04:05", stamp, time.Local)
	if err != nil {
		return stamp
	}
	seconds := time.Since(t).Seconds()
	for _, unit := range []struct {
		size float64
		name string
	}{{86400, " 天"}, {3600, " 小时"}, {60, " 分钟"}} {
		if seconds >= unit.size {
			return fmt.Sprintf("%d%s前", int(seconds/unit.size), unit.name)
		}
	}
	return fmt.Sprintf("%d 秒前", int(seconds))
}

func itemTable(title string) *ui.Table {
	return ui.NewTable(title, true, ui.Column{Header: "项目", Width: 14, Style: Dim}, ui.Column{Header: "状态"})
}

func SyncSection(cfg config.Config) (*ui.Table, error) {
	table := itemTable("同步会话")
	state, err := mutagen.Query(config.Session)
	if err != nil {
		return nil, err
	}
	if !state.Exists {
		if !state.DaemonRunning {
			table.Add("守护进程", ui.C("未运行 — 用 `mutagen daemon start` 启动", Bad))
		} else {
			table.Add("会话", ui.C(fmt.Sprintf("%q 不存在 — 用 `cc-runtime sync create` 创建", config.Session), Bad))
		}
		return table, nil
	}
	color := OK
	if !state.Healthy() {
		color = Bad
	}
	connected := ""
	if !state.Connected {
		connected = "（未连接）"
	}
	table.Add("状态", ui.C(state.Status+connected, color))
	table.Add("端点", cfg.Workspace+"  ⇄  "+cfg.LinuxWorkspace())
	table.Add("冲突", ui.C(fmt.Sprint(state.Conflicts), pick(state.Conflicts > 0, Bad, OK)))
	table.Add("问题", ui.C(fmt.Sprint(state.Problems), pick(state.Problems > 0, Bad, OK)))
	missing, extra := mutagen.IgnoreDrift(cfg, state)
	if len(missing) > 0 || len(extra) > 0 {
		var detail []string
		if len(missing) > 0 {
			detail = append(detail, "会话中缺少："+strings.Join(missing, ", "))
		}
		if len(extra) > 0 {
			detail = append(detail, "仅会话中有："+strings.Join(extra, ", "))
		}
		table.Add("忽略名单", ui.C("与配置不一致 — 重建会话后生效；"+strings.Join(detail, "；"), Warn))
	} else {
		table.Add("忽略名单", ui.C(fmt.Sprintf("%d 条，与配置一致", len(state.Ignores)), OK))
	}
	return table, nil
}

func pick(cond bool, yes, no ui.Style) ui.Style {
	if cond {
		return yes
	}
	return no
}

func num(m map[string]any, key string) int64 {
	if v, ok := m[key].(float64); ok {
		return int64(v)
	}
	return 0
}

func jobSummary(record map[string]any) ui.Cell {
	if record == nil {
		return ui.C("—", Dim)
	}
	summary, _ := record["summary"].(map[string]any)
	if message, ok := summary["error"].(string); ok {
		return ui.C(message, Bad)
	}
	if skipped, ok := summary["skipped"].(string); ok {
		return ui.C("跳过："+skipped, Warn)
	}
	switch record["job"] {
	case "dedupe":
		shared := num(summary, "shared_bytes") + num(summary, "already_bytes")
		text := fmt.Sprintf("已共享 %s（新增 %d，保持 %d），最近改动 %d", util.HumanBytes(float64(shared)),
			num(summary, "shared"), num(summary, "already"), num(summary, "recent"))
		if errors := num(summary, "errors"); errors > 0 {
			return ui.C(text+fmt.Sprintf("，错误 %d", errors), Bad)
		}
		return ui.C(text, ui.None)
	case "externalize":
		var moved []string
		if list, ok := summary["externalized"].([]any); ok {
			for _, item := range list {
				moved = append(moved, fmt.Sprint(item))
			}
		}
		text := fmt.Sprintf("迁出 %d", len(moved))
		if len(moved) > 0 {
			text += "：" + strings.Join(moved[:min(3, len(moved))], ", ")
		}
		if list, ok := summary["repaired"].([]any); ok && len(list) > 0 {
			text += fmt.Sprintf("，修正路径 %d", len(list))
		}
		return ui.C(text+fmt.Sprintf("，跳过 %d", num(summary, "skipped")), ui.None)
	}
	data, _ := json.Marshal(summary)
	text := string(data)
	if len(text) > 80 {
		text = text[:80]
	}
	return ui.C(text, ui.None)
}

func JobsSection(cfg config.Config) *ui.Table {
	table := ui.NewTable("定时任务", true,
		ui.Column{Header: "任务", Width: 12, Style: ui.Bold}, ui.Column{Header: "计划", Width: 12},
		ui.Column{Header: "launchd", Width: 8}, ui.Column{Header: "上次运行", Width: 12},
		ui.Column{Header: "退出码", Width: 6}, ui.Column{Header: "结果"})
	for _, job := range jobs.Jobs {
		present, loaded := jobs.Scheduled(job.Name)
		agent := ui.C("未安装", Dim)
		if loaded {
			agent = ui.C("已加载", OK)
		} else if present {
			agent = ui.C("仅有文件", Warn)
		}
		record := jobs.LastRun(cfg, job.Name)
		exit := ui.C("—", Dim)
		started := ""
		if record != nil {
			if code, ok := record["exit"].(float64); ok {
				exit = ui.C(fmt.Sprint(int(code)), pick(code == 0, OK, Bad))
			}
			started, _ = record["started"].(string)
		}
		table.Add(job.Name, job.ScheduleText(), agent, ago(started), exit, jobSummary(record))
	}
	return table
}

// RemoteShellSection 是 remote-shell 服务的状态；没有启用也没有安装时不显示。
func RemoteShellSection(cfg config.Config) *ui.Table {
	machines := remoteshell.Machines(cfg)
	state := remoteshell.State(cfg)
	if len(machines) == 0 && !state.Installed {
		return nil
	}
	table := itemTable("remote-shell")
	switch {
	case state.Pid > 0 && state.Version != config.Version:
		table.Add("服务", ui.C(fmt.Sprintf("运行中，但版本是 %s；用 cc-runtime remote-shell restart 重启", state.Version), Warn))
	case state.Pid > 0:
		table.Add("服务", ui.C(fmt.Sprintf("运行中（pid %d）", state.Pid), OK))
	case state.Loaded:
		table.Add("服务", ui.C("已加载，未运行", Bad))
	default:
		table.Add("服务", ui.C("未安装；用 cc-runtime remote-shell install 安装", Bad))
	}
	table.Add("开发机", ui.C(strings.Join(machines, ", "), ui.None))
	table.Add("进行中", ui.C(fmt.Sprintf("%d 条命令", len(state.Active)), ui.None))
	return table
}

func GitSection(cfg config.Config, full bool) *ui.Table {
	table := itemTable("Git 数据")
	table.Add("共享目录", cfg.GitStore())
	table.Add("仓库", fmt.Sprintf("已迁出 %d 个", len(gitstore.StoredGitdirs(cfg.GitStore()))))
	if !full {
		table.Add("检查", ui.C("运行 `cc-runtime status --git` 做完整扫描", Dim))
		return table
	}
	report := gitstore.Status(gitstore.FromConfig(cfg))
	for _, key := range report.SortedCounts() {
		table.Add("", fmt.Sprintf("%6d  %s", report.Counts[key], key))
	}
	for _, line := range report.Warnings {
		table.Add(ui.C("警告", Warn), line)
	}
	for _, line := range report.Anomalies {
		table.Add(ui.C("异常", Bad), line)
	}
	if len(report.Anomalies) == 0 {
		table.Add("异常", ui.C("无", OK))
	}
	return table
}

func DiskSection(cfg config.Config) (*ui.Table, error) {
	table := itemTable("磁盘")
	probe := cfg.Runtime
	if !util.Lexists(probe) {
		probe = cfg.Workspace
	}
	var st unix.Statfs_t
	if err := unix.Statfs(probe, &st); err != nil {
		return nil, &os.PathError{Op: "statfs", Path: probe, Err: err}
	}
	total := float64(st.Blocks) * float64(st.Bsize)
	free := float64(st.Bavail) * float64(st.Bsize)
	used := float64(st.Blocks-st.Bfree) * float64(st.Bsize)
	pct := used / total * 100
	table.Add("卷", ui.C(fmt.Sprintf("已用 %s，可用 %s（%.0f%%）", util.HumanBytes(used), util.HumanBytes(free), pct),
		pick(pct > 90, Bad, pick(pct > 80, Warn, ui.None))))
	if runtime.GOOS == "darwin" && util.Which("tmutil") != "" {
		result, _ := util.Run([]string{"tmutil", "listlocalsnapshots", "/"}, util.Opts{Capture: true, AllowFail: true})
		var names []string
		for _, line := range strings.Split(result.Stdout, "\n") {
			if line = strings.TrimSpace(line); strings.HasPrefix(line, "com.apple") {
				names = append(names, line)
			}
		}
		latest := "无"
		if len(names) > 0 {
			parts := strings.Split(names[len(names)-1], ".")
			if len(parts) >= 2 {
				latest = parts[len(parts)-2]
			}
		}
		table.Add("本地快照", fmt.Sprintf("%d 个（最新 %s）— Time Machine 删除前，它们会占着已释放的数据块", len(names), latest))
	}
	return table, nil
}

func ContainersSection(cfg config.Config) *ui.Table {
	docker := util.Which("docker")
	if docker == "" {
		table := ui.NewTable("容器", true, ui.Column{Header: "状态"})
		table.Add(ui.C("docker 不可用", Dim))
		return table
	}
	table := ui.NewTable("容器", true, ui.Column{Header: "服务"}, ui.Column{Header: "容器"},
		ui.Column{Header: "状态"}, ui.Column{Header: "备注"})
	project := filepath.Base(cfg.Root)
	out, _ := util.Run([]string{docker, "ps", "-a", "--filter", "label=com.docker.compose.project=" + project,
		"--format", "{{json .}}"}, util.Opts{Capture: true, AllowFail: true})
	services, _ := util.Run([]string{docker, "compose", "--project-directory", cfg.Root, "config", "--services"},
		util.Opts{Capture: true, AllowFail: true})
	configured := map[string]bool{}
	for _, name := range strings.Fields(services.Stdout) {
		configured[name] = true
	}
	type row struct{ Names, State, Status, Labels string }
	var rows []row
	for _, line := range strings.Split(out.Stdout, "\n") {
		var r row
		if strings.TrimSpace(line) != "" && json.Unmarshal([]byte(line), &r) == nil {
			rows = append(rows, r)
		}
	}
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && rows[j].Names < rows[j-1].Names; j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
	for _, r := range rows {
		service := "?"
		for _, item := range strings.Split(r.Labels, ",") {
			if key, value, ok := strings.Cut(item, "="); ok && key == "com.docker.compose.service" {
				service = value
			}
		}
		running := r.State == "running"
		if strings.HasSuffix(service, "network-init") && !running {
			continue // 一次性的辅助容器，退出是正常的
		}
		note := ui.C("", ui.None)
		if len(configured) > 0 && !configured[service] {
			note = ui.C("不在 compose.yaml 中（旧容器）", Warn)
		}
		table.Add(service, r.Names, ui.C(r.Status, pick(running, OK, Bad)), note)
	}
	return table
}

// Render 生成整个面板。某一部分出错不影响其他部分。
func Render(cfg config.Config, fullGit bool, width int) string {
	inner := width - 4
	var body []string
	add := func(build func() (*ui.Table, error)) {
		table, err := build()
		if table == nil && err == nil {
			return
		}
		if table != nil {
			table.Expand = true
		}
		if err != nil {
			for _, line := range ui.Wrap("出错："+err.Error(), inner) {
				body = append(body, Bad.S(line))
			}
		} else {
			body = append(body, table.Lines(inner)...)
		}
		body = append(body, "")
	}
	add(func() (*ui.Table, error) { return SyncSection(cfg) })
	add(func() (*ui.Table, error) { return JobsSection(cfg), nil })
	add(func() (*ui.Table, error) { return RemoteShellSection(cfg), nil })
	add(func() (*ui.Table, error) { return GitSection(cfg, fullGit), nil })
	add(func() (*ui.Table, error) { return DiskSection(cfg) })
	add(func() (*ui.Table, error) { return ContainersSection(cfg), nil })
	subtitle := time.Now().Format("2006-01-02 15:04:05") + " · " + config.Version
	return ui.Panel("cc-runtime status", subtitle, width, body)
}
