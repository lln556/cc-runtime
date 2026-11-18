// Package status 是 `status` 状态面板：remote-shell、定时任务、磁盘和容器放在一屏里。
package status

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zeke-chin/cc-runtime/internal/config"
	"github.com/zeke-chin/cc-runtime/internal/jobs"
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

func pick(cond bool, yes, no ui.Style) ui.Style {
	if cond {
		return yes
	}
	return no
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
	if len(jobs.Jobs) == 0 {
		table.Add(ui.C("没有登记的任务", Dim), "", "", "", "", "")
	}
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

// RemoteShellSection 是 remote-shell 服务的状态和已准备好的开发机。
func RemoteShellSection(cfg config.Config) *ui.Table {
	state := remoteshell.State(cfg)
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
	if machines := remoteshell.PreparedMachines(cfg); len(machines) > 0 {
		table.Add("开发机", ui.C(strings.Join(machines, ", "), ui.None))
	} else {
		table.Add("开发机", ui.C("还没有；cc-runtime up 时生成", Warn))
	}
	table.Add("进行中", ui.C(fmt.Sprintf("%d 条命令", len(state.Active)), ui.None))
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
func Render(cfg config.Config, width int) string {
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
	add(func() (*ui.Table, error) { return RemoteShellSection(cfg), nil })
	add(func() (*ui.Table, error) { return JobsSection(cfg), nil })
	add(func() (*ui.Table, error) { return DiskSection(cfg) })
	add(func() (*ui.Table, error) { return ContainersSection(cfg), nil })
	subtitle := time.Now().Format("2006-01-02 15:04:05") + " · " + config.Version
	return ui.Panel("cc-runtime status", subtitle, width, body)
}
