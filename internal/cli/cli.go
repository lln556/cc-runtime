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
	"github.com/zeke-chin/cc-runtime/internal/deploy"
	"github.com/zeke-chin/cc-runtime/internal/jobs"
	"github.com/zeke-chin/cc-runtime/internal/seed"
	"github.com/zeke-chin/cc-runtime/internal/status"
	"github.com/zeke-chin/cc-runtime/internal/ui"
	"github.com/zeke-chin/cc-runtime/internal/util"
)

const overview = `cc-runtime：开发运行环境的统一入口。

用法：cc-runtime <命令> [参数]

  status        状态面板：remote-shell、定时任务、磁盘、容器
  up            启动服务（先准备家目录、机器身份、缓存和 remote-shell）
  compose       对本部署执行任意 docker compose 命令
  seed-home     用已有家目录初始化新机器的家目录
  remote-shell  让开发机的 Bash 命令在 Mac 上执行的服务
  schedule      安装或移除 launchd 定时任务
  job           立即运行定时任务，或查看其日志
  check         一致性检查（网关配置、remote-shell）
  version       显示版本

每个命令都可以加 -h 查看说明。`

type command struct {
	usage string // 用法行（不含 "cc-runtime "）
	help  string
	run   func(cfg config.Config, args []string) (int, error)
}

var commands map[string]command

func init() {
	jobs.Exec = deploy.Exec
	commands = map[string]command{
		"status":    {"status [--watch[=秒]]", "整个运行环境的状态面板", cmdStatus},
		"up":        {"up [服务...] [--pull] [--force-recreate] [--allow-orphans]", "启动服务", cmdUp},
		"compose":   {"compose <docker compose 参数...>", "对本部署运行 docker compose", cmdCompose},
		"schedule":  {"schedule {list|install|uninstall} [任务...] [--all]", "launchd 定时任务", cmdSchedule},
		"job":       {"job {run|log} <任务> [-n 次数] [--json]", "立即运行任务，或查看其日志", cmdJob},
		"seed-home": {"seed-home <来源家目录> <目标家目录> [--dry-run] [--credentials-only]", "用已有家目录初始化新机器的家目录", cmdSeedHome},
		"check":     {"check {proxy|remote-shell|search-tools}", "一致性检查", cmdCheck},
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

// parse 允许选项和位置参数交错出现（例如 `schedule install foo --all`）。
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
	var watch watchFlag
	fs.Var(&watch, "watch", "持续刷新（默认每 5 秒，--watch=秒 指定间隔）")
	if _, err := parse("status", fs, args); err != nil {
		return 0, err
	}
	if watch.seconds == 0 {
		fmt.Println(status.Render(cfg, ui.TermWidth()))
		return 0, nil
	}
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	fmt.Print("\x1b[?1049h\x1b[?25l") // 备用屏幕，隐藏光标
	defer fmt.Print("\x1b[?25h\x1b[?1049l")
	for {
		frame := status.Render(cfg, ui.TermWidth())
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
		if len(jobs.Jobs) == 0 {
			fmt.Println(ui.Dim.S("没有登记的任务。"))
			return 0, nil
		}
		table := ui.NewTable("", true, ui.Column{Header: "任务"}, ui.Column{Header: "计划"},
			ui.Column{Header: "launchd"}, ui.Column{Header: "说明"})
		stale := false
		for _, job := range jobs.Jobs {
			present, loaded := jobs.Scheduled(job.Name)
			state := ui.C("未安装", ui.Dim)
			if loaded {
				state = ui.C("已加载", ui.Green)
			} else if present {
				state = ui.C("仅有文件", ui.Yellow)
			}
			if present && jobs.Stale(cfg, job.Name) {
				state = ui.C("需重装", ui.Yellow)
				stale = true
			}
			table.Add(job.Name, job.ScheduleText(cfg), state, job.Description)
		}
		table.Print()
		if stale {
			fmt.Println(ui.Yellow.S("已安装的 launchd 定义和当前配置不一致（配置、Mac 的时区或夏令时变了），请运行 cc-runtime schedule install --all"))
		}
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
			fmt.Printf("%s %s（%s）：%s\n", ui.Green.S("已安装"), name, job.ScheduleText(cfg), jobs.PlistPath(name))
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
	what, err := choose("check", "检查项", positional, "proxy", "remote-shell", "search-tools")
	if err != nil {
		return 0, err
	}
	switch what {
	case "remote-shell":
		return checkRemoteShell(cfg)
	case "search-tools":
		return checkSearchTools(cfg)
	}
	results, err := checks.ProxyDifferences(filepath.Join(cfg.Root, "proxy"))
	if err != nil {
		return 0, err
	}
	failed := false
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
