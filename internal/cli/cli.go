// Package cli 是 cc-runtime 的命令行：解析参数，把各命令的结果打印出来。
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zeke-chin/cc-runtime/internal/checks"
	"github.com/zeke-chin/cc-runtime/internal/config"
	"github.com/zeke-chin/cc-runtime/internal/dedupe"
	"github.com/zeke-chin/cc-runtime/internal/deploy"
	"github.com/zeke-chin/cc-runtime/internal/gitstore"
	"github.com/zeke-chin/cc-runtime/internal/jobs"
	"github.com/zeke-chin/cc-runtime/internal/mutagen"
	"github.com/zeke-chin/cc-runtime/internal/seed"
	"github.com/zeke-chin/cc-runtime/internal/status"
	"github.com/zeke-chin/cc-runtime/internal/ui"
	"github.com/zeke-chin/cc-runtime/internal/util"
)

const overview = `cc-runtime：开发运行环境的统一入口。

用法：cc-runtime <命令> [参数]

  status      状态面板：同步会话、定时任务、git 数据、磁盘、容器
  up          启动服务（先准备家目录、机器身份和缓存）
  compose     对本部署执行任意 docker compose 命令
  sync        Mac 工作区与 Linux 副本之间的 Mutagen 会话
  git         把仓库的 git 数据放到共享目录
  dedupe      在两棵目录树之间共享相同文件
  schedule    安装或移除 launchd 任务
  job         立即运行定时任务，或查看其日志
  seed-home   用已有家目录初始化新机器的家目录
  remote-shell  让开发机的 Bash 命令在 Mac 上执行的服务
  check       一致性检查（同步忽略名单、网关配置、remote-shell）
  version     显示版本

每个命令都可以加 -h 查看说明。`

type command struct {
	usage string // 用法行（不含 "cc-runtime "）
	help  string
	run   func(cfg config.Config, args []string) (int, error)
}

var commands map[string]command

func init() {
	commands = map[string]command{
		"status":    {"status [--git] [--watch[=秒]]", "整个运行环境的状态面板", cmdStatus},
		"up":        {"up [服务...] [--pull] [--force-recreate] [--allow-orphans]", "启动服务", cmdUp},
		"compose":   {"compose <docker compose 参数...>", "对本部署运行 docker compose", cmdCompose},
		"sync":      {"sync {status|create|compare|ignores} [--content] [-v]", "Mutagen 同步会话", cmdSync},
		"git":       {"git {status|externalize|restore} [仓库...] [--all] [--dry-run] [--list]", "共享目录中的 git 数据", cmdGit},
		"dedupe":    {"dedupe [--dry-run] [--min-size 字节] [--quiet-hours 小时] [--force]", "在两棵目录树之间共享相同文件", cmdDedupe},
		"schedule":  {"schedule {list|install|uninstall} [任务...] [--all]", "launchd 定时任务", cmdSchedule},
		"job":       {"job {run|log} <任务> [-n 次数] [--json]", "立即运行任务，或查看其日志", cmdJob},
		"seed-home": {"seed-home <来源家目录> <目标家目录> [--dry-run] [--credentials-only]", "用已有家目录初始化新机器的家目录", cmdSeedHome},
		"check":     {"check {ignores|proxy|remote-shell}", "一致性检查", cmdCheck},
		"remote-shell": {"remote-shell {status|install|uninstall|restart|serve} [--force]",
			"让开发机的 Bash 命令在 Mac 上执行的服务（LaunchAgent）", cmdRemoteShell},
	}
}

// errHelp 表示已经打印了帮助，正常退出。
var errHelp = errors.New("help")

// newFlags 建一个子命令的参数集；-h 时打印中文用法。
func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func printUsage(name string, fs *flag.FlagSet) {
	c := commands[name]
	fmt.Printf("用法：cc-runtime %s\n\n%s\n", c.usage, c.help)
	var lines []string
	fs.VisitAll(func(f *flag.Flag) {
		if f.Usage == "" {
			return
		}
		prefix := "--"
		if len(f.Name) == 1 {
			prefix = "-"
		}
		lines = append(lines, fmt.Sprintf("  %-20s %s", prefix+f.Name, f.Usage))
	})
	if len(lines) > 0 {
		fmt.Println("\n选项：")
		fmt.Println(strings.Join(lines, "\n"))
	}
}

// parse 允许选项和位置参数交错出现（例如 `git externalize foo --dry-run`）。
func parse(name string, fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				printUsage(name, fs)
				return nil, errHelp
			}
			return nil, util.Errorf("%s：%v（用 cc-runtime %s -h 查看用法）", name, translate(err), name)
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		if args[0] == "--" {
			return append(positional, args[1:]...), nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

func translate(err error) string {
	text := err.Error()
	text = strings.Replace(text, "flag provided but not defined:", "未知选项", 1)
	text = strings.Replace(text, "flag needs an argument:", "选项缺少值", 1)
	text = strings.Replace(text, "invalid value", "无效的值", 1)
	return text
}

func choose(name, what string, value []string, choices ...string) (string, error) {
	if len(value) == 0 {
		return "", util.Errorf("%s：缺少%s，可选：%s", name, what, strings.Join(choices, "、"))
	}
	if !slices.Contains(choices, value[0]) {
		return "", util.Errorf("%s：无效的%s %q，可选：%s", name, what, value[0], strings.Join(choices, "、"))
	}
	return value[0], nil
}

// ---- status ----------------------------------------------------------------------

// watchFlag：`--watch` 表示每 5 秒刷新，`--watch=10` 指定间隔。
type watchFlag struct{ seconds float64 }

func (w *watchFlag) String() string   { return "" }
func (w *watchFlag) IsBoolFlag() bool { return true }
func (w *watchFlag) Set(value string) error {
	if value == "true" {
		w.seconds = 5
		return nil
	}
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil || seconds <= 0 {
		return errors.New("应为正数秒")
	}
	w.seconds = seconds
	return nil
}

func cmdStatus(cfg config.Config, args []string) (int, error) {
	fs := newFlags("status")
	git := fs.Bool("git", false, "同时扫描所有仓库（较慢）")
	var watch watchFlag
	fs.Var(&watch, "watch", "持续刷新（默认每 5 秒，--watch=秒 指定间隔）")
	if _, err := parse("status", fs, args); err != nil {
		return 0, err
	}
	if watch.seconds == 0 {
		fmt.Println(status.Render(cfg, *git, ui.TermWidth()))
		return 0, nil
	}
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	fmt.Print("\x1b[?1049h\x1b[?25l") // 备用屏幕，隐藏光标
	defer fmt.Print("\x1b[?25h\x1b[?1049l")
	for {
		frame := status.Render(cfg, *git, ui.TermWidth())
		fmt.Print("\x1b[H\x1b[2J" + frame)
		select {
		case <-interrupt:
			return 0, nil
		case <-time.After(time.Duration(watch.seconds * float64(time.Second))):
		}
	}
}

// ---- 部署 --------------------------------------------------------------------

func cmdUp(cfg config.Config, args []string) (int, error) {
	fs := newFlags("up")
	pull := fs.Bool("pull", false, "先拉取镜像")
	force := fs.Bool("force-recreate", false, "强制重建容器")
	orphans := fs.Bool("allow-orphans", false, "即使有不在 compose.yaml 里的容器在运行也启动")
	services, err := parse("up", fs, args)
	if err != nil {
		return 0, err
	}
	return 0, deploy.Up(cfg, services, deploy.UpOptions{Pull: *pull, ForceRecreate: *force, AllowOrphans: *orphans})
}

func cmdCompose(cfg config.Config, args []string) (int, error) {
	if len(args) == 0 {
		return 0, util.Errorf("请给出 docker compose 命令，例如 `cc-runtime compose ps`")
	}
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		printUsage("compose", newFlags("compose"))
		return 0, nil
	}
	return deploy.ComposePassthrough(cfg, args)
}

func cmdSeedHome(cfg config.Config, args []string) (int, error) {
	fs := newFlags("seed-home")
	dryRun := fs.Bool("dry-run", false, "只显示将要做的操作")
	credentials := fs.Bool("credentials-only", false, "只把共享凭据复制到已有家目录")
	positional, err := parse("seed-home", fs, args)
	if err != nil {
		return 0, err
	}
	if len(positional) != 2 {
		return 0, util.Errorf("seed-home：需要来源和目标两个家目录")
	}
	return 0, seed.Seed(positional[0], positional[1], seed.Options{DryRun: *dryRun,
		CredentialsOnly: *credentials, Out: func(s string) { fmt.Println(s) }})
}

// ---- 同步 --------------------------------------------------------------------

func cmdSync(cfg config.Config, args []string) (int, error) {
	fs := newFlags("sync")
	content := fs.Bool("content", false, "compare：同时比较文件内容")
	verbose := fs.Bool("verbose", false, "status：显示 Mutagen 的完整输出")
	fs.BoolVar(verbose, "v", false, "同 --verbose")
	positional, err := parse("sync", fs, args)
	if err != nil {
		return 0, err
	}
	action, err := choose("sync", "操作", positional, "status", "create", "compare", "ignores")
	if err != nil {
		return 0, err
	}
	switch action {
	case "create":
		if err := mutagen.Create(cfg); err != nil {
			return 0, err
		}
		fmt.Printf("%s用 `cc-runtime sync status` 查看首次同步进度。\n", ui.Green.S(fmt.Sprintf("已创建会话 %q。", config.Session)))
		return 0, nil
	case "ignores":
		for _, entry := range cfg.Ignores().Entries {
			fmt.Println(entry)
		}
		return 0, nil
	case "compare":
		mutagen.Flush()
		done := ui.Progress("正在比较两棵目录树…")
		differences, err := mutagen.Compare(cfg.Workspace, cfg.LinuxWorkspace(), cfg.Ignores(), *content)
		done()
		if err != nil {
			return 0, err
		}
		for _, line := range differences[:min(200, len(differences))] {
			fmt.Println(line)
		}
		if len(differences) > 200 {
			fmt.Printf("… 还有 %d 处\n", len(differences)-200)
		}
		if len(differences) > 0 {
			fmt.Println(ui.Red.S(fmt.Sprintf("共 %d 处差异", len(differences))))
			return 1, nil
		}
		fmt.Println(ui.Green.S("忽略名单之外，两棵目录树一致。"))
		return 0, nil
	}
	state, err := mutagen.Query(config.Session)
	if err != nil {
		return 0, err
	}
	if !state.Exists {
		if !state.DaemonRunning {
			fmt.Println(ui.Red.S("Mutagen 守护进程未运行。") + "用 `mutagen daemon start` 启动。")
		} else {
			fmt.Println(ui.Red.S(fmt.Sprintf("会话 %q 不存在。", config.Session)) + "用 `cc-runtime sync create` 创建。")
		}
		return 1, nil
	}
	if *verbose {
		fmt.Println(strings.TrimSpace(state.Raw))
	}
	color := ui.Green
	if !state.OK() {
		color = ui.Red
	}
	fmt.Printf("%s · 冲突 %d · 问题 %d\n", color.S(state.Status), state.Conflicts, state.Problems)
	if missing, extra := mutagen.IgnoreDrift(cfg, state); len(missing) > 0 || len(extra) > 0 {
		fmt.Println(ui.Yellow.S("会话的忽略名单与配置不一致；重建会话后才会生效。"))
	}
	if state.OK() {
		return 0, nil
	}
	return 1, nil
}

// ---- git -----------------------------------------------------------------------

func cmdGit(cfg config.Config, args []string) (int, error) {
	fs := newFlags("git")
	all := fs.Bool("all", false, "externalize/restore：所有符合条件的仓库")
	dryRun := fs.Bool("dry-run", false, "externalize/restore：只显示将要做的操作")
	list := fs.Bool("list", false, "status：列出已迁出的仓库")
	positional, err := parse("git", fs, args)
	if err != nil {
		return 0, err
	}
	action, err := choose("git", "操作", positional, "status", "externalize", "restore")
	if err != nil {
		return 0, err
	}
	repos := positional[1:]
	layout := gitstore.FromConfig(cfg)
	if err := layout.Check(); err != nil {
		return 0, util.Errorf("%v", err)
	}
	if action == "status" {
		done := ui.Progress("正在扫描两棵目录树…")
		report := gitstore.Status(layout)
		done()
		table := ui.NewTable("", false, ui.Column{}, ui.Column{})
		for _, key := range report.SortedCounts() {
			table.Add(fmt.Sprintf("%6d", report.Counts[key]), key)
		}
		table.Print()
		if *list {
			for _, rel := range report.Externalized {
				fmt.Println("  " + rel)
			}
		}
		for _, line := range report.Warnings {
			fmt.Println(ui.Yellow.S("警告") + "  " + line)
		}
		for _, line := range report.Anomalies {
			fmt.Println(ui.Red.S("异常") + "  " + line)
		}
		if len(report.Anomalies) > 0 {
			return 1, nil
		}
		fmt.Println(ui.Green.S("没有异常。"))
		return 0, nil
	}
	if *all == (len(repos) > 0) {
		return 0, util.Errorf("请指定仓库或 --all，二者只能选一")
	}
	var targets []string
	if *all {
		scanned := gitstore.Scan(layout)
		for _, rel := range gitstore.SortedRels(scanned) {
			if (action == "externalize" && scanned[rel].Has("dir")) || (action == "restore" && scanned[rel].Mac == "link") {
				targets = append(targets, rel)
			}
		}
	} else {
		for _, repo := range repos {
			rel, err := layout.Rel(repo)
			if err != nil {
				return 0, util.Errorf("%v", err)
			}
			targets = append(targets, rel)
		}
		slices.Sort(targets)
	}
	operation := gitstore.Externalize
	if action == "restore" {
		operation = gitstore.Restore
	}
	failed := 0
	for _, rel := range targets {
		changed, message, err := operation(layout, rel, *dryRun)
		if err != nil {
			changed, message = false, fmt.Sprintf("%s：出错：%v", rel, err)
		}
		fmt.Println(pickStyle(changed).S(message))
		// 用 --all 时，跳过（忙）是预期的；明确指定的仓库必须成功。
		if !changed && !*all && !strings.HasSuffix(message, gitstore.AlreadyExternalized) {
			failed++
		}
	}
	if failed > 0 {
		return 1, nil
	}
	return 0, nil
}

func pickStyle(ok bool) ui.Style {
	if ok {
		return ui.Green
	}
	return ui.Yellow
}

// ---- 去重 ------------------------------------------------------------------------

func cmdDedupe(cfg config.Config, args []string) (int, error) {
	fs := newFlags("dedupe")
	dryRun := fs.Bool("dry-run", false, "只报告可共享的内容")
	minSize := fs.Int64("min-size", dedupe.MiB, "跳过小于此大小的文件（字节）")
	quietHours := fs.Float64("quiet-hours", 24, "跳过任一侧在这么多小时内改动过的文件")
	force := fs.Bool("force", false, "即使同步会话状态不正常也运行")
	if _, err := parse("dedupe", fs, args); err != nil {
		return 0, err
	}
	if !*force {
		state, err := mutagen.Query(config.Session)
		if err != nil {
			return 0, err
		}
		if !state.OK() {
			status := state.Status
			if status == "" {
				status = "未运行"
			}
			fmt.Println(ui.Yellow.S(fmt.Sprintf("不去重：同步会话状态不正常（%s，冲突 %d）。", status, state.Conflicts)) +
				"如仍要运行，加 --force。")
			return 2, nil
		}
	}
	done := ui.Progress("正在比较文件…")
	stats, err := dedupe.Run(cfg, dedupe.RunOptions{DryRun: *dryRun, MinSize: *minSize, QuietHours: *quietHours,
		Log: func(m string) { done(); fmt.Println(ui.Red.S(m)) }})
	done()
	var unsupported *dedupe.Unsupported
	if errors.As(err, &unsupported) {
		return 0, util.Errorf("%s", unsupported.Msg)
	}
	if err != nil {
		return 0, err
	}
	table := ui.NewTable("", false, ui.Column{}, ui.Column{})
	if *dryRun {
		table.Add("可共享", fmt.Sprintf("%d 个文件，%s", stats.WouldShare, util.HumanBytes(float64(stats.WouldShareBytes))))
	} else {
		table.Add("本次共享", fmt.Sprintf("%d 个文件，%s", stats.Shared, util.HumanBytes(float64(stats.SharedBytes))))
	}
	table.Add("此前已共享", fmt.Sprintf("%d 个文件，%s", stats.Already, util.HumanBytes(float64(stats.AlreadyBytes))))
	table.Add("跳过", fmt.Sprintf("最近改动 %d，内容不同 %d，大小不同 %d，仅一侧存在 %d，过小 %d",
		stats.Recent, stats.Differs, stats.SizeDiffers, stats.Missing, stats.Small))
	table.Add("错误", ui.C(fmt.Sprint(stats.Errors), pickErr(stats.Errors > 0)))
	table.Add("耗时", fmt.Sprintf("%.1f 秒，方式 %s", stats.Seconds, stats.Method))
	table.Print()
	if !*dryRun && stats.SharedBytes > 0 {
		fmt.Println(ui.Dim.S("持有旧数据块的本地快照删除后，释放的空间才会在 df 中体现。"))
	}
	if stats.Errors > 0 {
		return 1, nil
	}
	return 0, nil
}

func pickErr(bad bool) ui.Style {
	if bad {
		return ui.Red
	}
	return ui.Green
}

// ---- 定时任务 ----------------------------------------------------------------------

func cmdSchedule(cfg config.Config, args []string) (int, error) {
	fs := newFlags("schedule")
	all := fs.Bool("all", false, "所有任务")
	positional, err := parse("schedule", fs, args)
	if err != nil {
		return 0, err
	}
	action, err := choose("schedule", "操作", positional, "list", "install", "uninstall")
	if err != nil {
		return 0, err
	}
	if action == "list" {
		table := ui.NewTable("", true, ui.Column{Header: "任务"}, ui.Column{Header: "计划"},
			ui.Column{Header: "launchd"}, ui.Column{Header: "说明"})
		for _, job := range jobs.Jobs {
			present, loaded := jobs.Scheduled(job.Name)
			state := ui.C("未安装", ui.Dim)
			if loaded {
				state = ui.C("已加载", ui.Green)
			} else if present {
				state = ui.C("仅有文件", ui.Yellow)
			}
			table.Add(job.Name, job.ScheduleText(), state, job.Description)
		}
		table.Print()
		return 0, nil
	}
	names := positional[1:]
	if *all == (len(names) > 0) {
		return 0, util.Errorf("请指定任务名或 --all，二者只能选一")
	}
	if *all {
		names = jobs.Names()
	}
	for _, name := range names {
		if _, err := jobs.Get(name); err != nil {
			return 0, err
		}
	}
	if action == "install" {
		if err := jobs.Install(cfg, names); err != nil {
			return 0, err
		}
		for _, name := range names {
			job, _ := jobs.Get(name)
			fmt.Printf("%s %s（%s）：%s\n", ui.Green.S("已安装"), name, job.ScheduleText(), jobs.PlistPath(name))
		}
		return 0, nil
	}
	if err := jobs.Uninstall(names); err != nil {
		return 0, err
	}
	for _, name := range names {
		fmt.Printf("已移除 %s。\n", name)
	}
	return 0, nil
}

func cmdJob(cfg config.Config, args []string) (int, error) {
	fs := newFlags("job")
	lines := fs.Int("lines", 10, "log：显示最近几次运行")
	fs.IntVar(lines, "n", 10, "同 --lines")
	asJSON := fs.Bool("json", false, "log：格式化显示每条记录")
	positional, err := parse("job", fs, args)
	if err != nil {
		return 0, err
	}
	action, err := choose("job", "操作", positional, "run", "log")
	if err != nil {
		return 0, err
	}
	name, err := choose("job", "任务", positional[1:], jobs.Names()...)
	if err != nil {
		return 0, err
	}
	if action == "log" {
		path := filepath.Join(cfg.LogDir(), name+".log")
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			fmt.Println(ui.Dim.S("还没有日志：" + path))
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
		all := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		for _, line := range all[max(len(all)-*lines, 0):] {
			if *asJSON {
				ui.PrintJSON(line)
			} else {
				fmt.Println(line)
			}
		}
		return 0, nil
	}
	record, err := jobs.RunJob(cfg, name)
	if err != nil {
		return 0, err
	}
	ui.PrintJSON(record)
	return record.Exit, nil
}

// ---- 检查 ------------------------------------------------------------------------------

func cmdCheck(cfg config.Config, args []string) (int, error) {
	fs := newFlags("check")
	positional, err := parse("check", fs, args)
	if err != nil {
		return 0, err
	}
	what, err := choose("check", "检查项", positional, "ignores", "proxy", "remote-shell")
	if err != nil {
		return 0, err
	}
	if what == "remote-shell" {
		return checkRemoteShell(cfg)
	}
	failed := false
	if what == "proxy" {
		results, err := checks.ProxyDifferences(filepath.Join(cfg.Root, "proxy"))
		if err != nil {
			return 0, err
		}
		for _, result := range results {
			if len(result.Lines) == 0 {
				fmt.Printf("%s    %s\n", ui.Green.S("ok"), result.Name)
				continue
			}
			failed = true
			fmt.Printf("%s  %s 在 GATEWAY-SPECIFIC 块之外与 config.example.yaml 不一致：\n", ui.Red.S("DIFF"), result.Name)
			for _, line := range result.Lines {
				fmt.Println("      " + line)
			}
		}
		return exitCode(failed), nil
	}
	ignores := cfg.Ignores()
	for _, message := range checks.LayoutErrors(cfg) {
		fmt.Println(ui.Red.S("ERROR") + "  目录布局：" + message)
		failed = true
	}
	done := ui.Progress("正在列出每个仓库的已跟踪文件…")
	violations := checks.TrackedViolations(cfg.Workspace, ignores)
	done()
	for _, v := range violations {
		fmt.Printf("%s  已跟踪的文件被忽略：%s/%s\n", ui.Red.S("ERROR"), v.Repo, v.Name)
		failed = true
	}
	mounts, err := checks.ContainerStateMounts(cfg.Workspace, ignores)
	if err != nil {
		fmt.Printf("%s   无法列出容器挂载：%v\n", ui.Yellow.S("WARN"), err)
	}
	for _, m := range mounts {
		fmt.Printf("%s  容器 %s 以可写方式挂载 %s；请加入忽略名单\n", ui.Red.S("ERROR"), m.Container, m.Rel)
		failed = true
	}
	for _, rel := range checks.StrayVirtualenvs(cfg.Workspace, ignores) {
		fmt.Printf("%s   虚拟环境会被同步：%s\n", ui.Yellow.S("WARN"), rel)
	}
	if failed {
		fmt.Println(ui.Red.S("FAILED"))
	} else {
		fmt.Println(ui.Green.S("OK"))
	}
	return exitCode(failed), nil
}

func exitCode(failed bool) int {
	if failed {
		return 1
	}
	return 0
}

// Main 运行命令并返回退出码。
func Main(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Println(overview)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	if args[0] == "version" || args[0] == "--version" {
		root, _ := config.FindRoot()
		fmt.Printf("cc-runtime %s（仓库 %s）\n", config.Version, root)
		return 0
	}
	c, ok := commands[args[0]]
	if !ok {
		ui.Errln(ui.Red.E("错误：") + fmt.Sprintf("未知命令 %q；运行 cc-runtime -h 查看所有命令", args[0]))
		return 2
	}
	root, err := config.FindRoot()
	if err != nil {
		ui.Errln(ui.Red.E("错误：") + err.Error())
		return 1
	}
	code, err := c.run(config.Load(root, nil), args[1:])
	if errors.Is(err, errHelp) {
		return 0
	}
	if err != nil {
		ui.Errln(ui.Red.E("错误：") + err.Error())
		if code == 0 {
			code = 1
		}
	}
	return code
}
