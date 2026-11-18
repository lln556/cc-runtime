package cli

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zeke-chin/cc-runtime/internal/config"
	"github.com/zeke-chin/cc-runtime/internal/deploy"
	"github.com/zeke-chin/cc-runtime/internal/remoteshell"
	"github.com/zeke-chin/cc-runtime/internal/ui"
)

func cmdRemoteShell(cfg config.Config, args []string) (int, error) {
	fs := newFlags("remote-shell")
	force := fs.Bool("force", false, "restart：即使有命令正在运行也重启（会中止它们）")
	positional, err := parse("remote-shell", fs, args)
	if err != nil {
		return 0, err
	}
	action, err := choose("remote-shell", "操作", positional, "status", "install", "uninstall", "restart", "serve")
	if err != nil {
		return 0, err
	}
	switch action {
	case "serve":
		return 0, remoteshell.Serve(cfg)
	case "install":
		if err := remoteshell.Install(cfg); err != nil {
			return 0, err
		}
		fmt.Printf("%s %s\n", ui.Green.S("已安装并启动"), remoteshell.PlistPath())
		return 0, nil
	case "uninstall":
		if err := remoteshell.Uninstall(); err != nil {
			return 0, err
		}
		fmt.Println("已停止并移除 remote-shell 服务。")
		return 0, nil
	case "restart":
		if err := remoteshell.Restart(cfg, *force); err != nil {
			return 0, err
		}
		fmt.Println(ui.Green.S("已重启 remote-shell 服务。"))
		return 0, nil
	}
	state := remoteshell.State(cfg)
	port, _ := remoteshell.Port(cfg)
	table := ui.NewTable("", false, ui.Column{}, ui.Column{})
	service := ui.C("未安装", ui.Dim)
	switch {
	case state.Pid > 0:
		service = ui.C(fmt.Sprintf("运行中（pid %d，版本 %s）", state.Pid, state.Version), ui.Green)
	case state.Loaded:
		service = ui.C("已加载，未运行", ui.Yellow)
	case state.Installed:
		service = ui.C("仅有文件", ui.Yellow)
	}
	table.Add("服务", service)
	table.Add("监听", fmt.Sprintf("127.0.0.1:%d", port))
	table.Add("开发机", strings.Join(remoteshell.PreparedMachines(cfg), ", "))
	table.Add("进行中", fmt.Sprint(len(state.Active)))
	table.Print()
	for _, a := range state.Active {
		fmt.Printf("  %s pid=%d %s  %s\n", a.Machine, a.Pid, time.Since(a.Started).Round(time.Second), a.Command)
	}
	return 0, nil
}

type checkReport struct{ failed bool }

func (r *checkReport) ok(format string, args ...any) {
	fmt.Printf("%s    %s\n", ui.Green.S("ok"), fmt.Sprintf(format, args...))
}
func (r *checkReport) warn(format string, args ...any) {
	fmt.Printf("%s  %s\n", ui.Yellow.S("WARN"), fmt.Sprintf(format, args...))
}
func (r *checkReport) fail(format string, args ...any) {
	r.failed = true
	fmt.Printf("%s %s\n", ui.Red.S("ERROR"), fmt.Sprintf(format, args...))
}

// checkRemoteShell 检查服务、网关 tunnel、开发机的挂载和 Claude Code 配置，并端到端执行一条命令。
func checkRemoteShell(cfg config.Config) (int, error) {
	r := &checkReport{}
	port, err := remoteshell.Port(cfg)
	if err != nil {
		return 0, err
	}
	state := remoteshell.State(cfg)
	switch {
	case state.Pid == 0:
		r.fail("服务没有运行；用 cc-runtime remote-shell install 安装")
	case state.Version != config.Version:
		r.fail("运行中的服务是 %s，已安装的是 %s；用 cc-runtime remote-shell restart 重启", state.Version, config.Version)
	default:
		r.ok("服务运行中（pid %d，版本 %s）", state.Pid, state.Version)
	}
	if conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 2*time.Second); err != nil {
		r.fail("连不上 127.0.0.1:%d：%v", port, err)
	} else {
		conn.Close()
	}
	binary := filepath.Join(remoteshell.BinSource(cfg), remoteshell.LinuxBinary)
	if _, err := os.Stat(binary); err != nil {
		r.fail("没有开发机用的二进制 %s；在 Mac 上运行 scripts/install.sh", binary)
	}
	project, err := deploy.Model(cfg)
	if err != nil {
		return 0, err
	}
	containers, err := deploy.RunningContainers(project)
	if err != nil {
		return 0, err
	}
	legacy := deploy.LegacyMachines(containers, filepath.Join(cfg.Runtime, "linux-workspace"))
	for _, machine := range deploy.DevServices(project) {
		if legacy[machine] {
			r.warn("%s：还挂着 Linux 副本，迁移前不检查（见 docs/local/redesign/03）", machine)
			continue
		}
		checkMachine(cfg, r, project, machine, project.Services[machine], port)
	}
	return exitCode(r.failed), nil
}

func checkMachine(cfg config.Config, r *checkReport, project deploy.Project, machine string, service deploy.Service, port int) {
	gateway := deploy.GatewayIP(project, machine)
	code := strings.TrimPrefix(machine, "dev-")
	proxyConfig := filepath.Join(cfg.Root, "proxy", "config-"+code+".yaml")
	tunnel := regexp.MustCompile(`address:\s*` + regexp.QuoteMeta(gateway) + `:` + strconv.Itoa(remoteshell.TunnelPort) +
		`\s*\n\s*target:\s*host\.docker\.internal:` + strconv.Itoa(port) + `\s*\n`)
	if data, err := os.ReadFile(proxyConfig); err != nil {
		r.fail("%s：读不到网关配置 %s", machine, proxyConfig)
	} else if !tunnel.Match(data) {
		r.fail("%s：网关配置 %s 里没有 tunnel %s:%d → host.docker.internal:%d", machine, filepath.Base(proxyConfig),
			gateway, remoteshell.TunnelPort, port)
	} else {
		r.ok("%s：网关 tunnel %s:%d → host.docker.internal:%d", machine, gateway, remoteshell.TunnelPort, port)
	}
	if _, err := os.Stat(remoteshell.ClientConfigPath(cfg, machine)); err != nil {
		r.fail("%s：没有 %s；用 cc-runtime up %s 生成", machine, remoteshell.ClientConfigPath(cfg, machine), machine)
	}
	if mount := deploy.MountFor(service, filepath.Clean(cfg.Workspace)); mount == nil || filepath.Clean(mount.Source) != filepath.Clean(cfg.Workspace) {
		r.fail("%s：compose.yaml 里的工作区挂载源应为 %s", machine, cfg.Workspace)
	}
	if home := deploy.MountFor(service, deploy.HomeTarget); home != nil {
		checkClaudeConfig(cfg, r, machine, home.Source)
	}
	result, err := deploy.Exec(cfg, machine, "sh", "-c", `cd "$1" && exec "$2" -c 'uname -s; launchctl managername'`,
		"sh", cfg.Workspace, remoteshell.ShellPath)
	output := strings.Join(strings.Fields(result.Stdout+" "+result.Stderr), " ")
	switch {
	case err != nil || result.Code != 0:
		r.fail("%s：在开发机里执行命令失败（退出码 %d）：%s", machine, result.Code, output)
	case output != "Darwin Aqua":
		r.fail("%s：命令的执行环境不对：%s（应为 Darwin Aqua）", machine, output)
	default:
		r.ok("%s：命令在 Mac 的登录会话里执行（%s）", machine, output)
	}
}

// checkClaudeConfig 在开发机里检查 Claude Code 的系统级设置，并运行一次 SessionStart hook。
func checkClaudeConfig(cfg config.Config, r *checkReport, machine, home string) {
	read := func(path string) (string, bool) {
		result, err := deploy.Exec(cfg, machine, "cat", path)
		if err != nil || result.Code != 0 {
			r.fail("%s：开发机里读不到 %s", machine, path)
			return "", false
		}
		return result.Stdout, true
	}
	var managed struct {
		Env   map[string]string `json:"env"`
		Hooks map[string]any    `json:"hooks"`
	}
	if text, ok := read("/etc/claude-code/managed-settings.json"); ok {
		err := json.Unmarshal([]byte(text), &managed)
		hooks, _ := json.Marshal(managed.Hooks)
		if err != nil {
			r.fail("%s：/etc/claude-code/managed-settings.json 格式错误：%v", machine, err)
		} else if managed.Env["CLAUDE_CODE_SHELL"] != remoteshell.ShellPath || !strings.Contains(string(hooks), remoteshell.SessionStartPath) {
			r.fail("%s：/etc/claude-code/managed-settings.json 应设置 CLAUDE_CODE_SHELL=%s 和 SessionStart hook %s",
				machine, remoteshell.ShellPath, remoteshell.SessionStartPath)
		} else {
			r.ok("%s：系统级设置（CLAUDE_CODE_SHELL、SessionStart hook）", machine)
		}
	}
	var dropin struct {
		Env map[string]string `json:"env"`
	}
	if text, ok := read("/etc/claude-code/managed-settings.d/remote-shell.json"); ok {
		if json.Unmarshal([]byte(text), &dropin) != nil || dropin.Env["CLAUDE_CODE_TMPDIR"] != remoteshell.TmpDir(cfg, machine) {
			r.fail("%s：managed-settings.d/remote-shell.json 的 CLAUDE_CODE_TMPDIR 应为 %s（entrypoint 按 CC_REMOTE_SHELL_TMPDIR 写入）",
				machine, remoteshell.TmpDir(cfg, machine))
		} else {
			r.ok("%s：CLAUDE_CODE_TMPDIR=%s", machine, dropin.Env["CLAUDE_CODE_TMPDIR"])
		}
	}
	if text, ok := read("/etc/claude-code/CLAUDE.md"); ok && !strings.Contains(text, "remote-shell") {
		r.fail("%s：/etc/claude-code/CLAUDE.md 没有说明 Bash 在 Mac 上执行", machine)
	}
	input, _ := json.Marshal(map[string]string{"cwd": cfg.Workspace, "source": "startup", "hook_event_name": "SessionStart"})
	result, err := deploy.Exec(cfg, machine, "sh", "-c", `printf '%s' "$1" | "$2"`, "sh", string(input), remoteshell.SessionStartPath)
	var hook struct {
		Output struct {
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	switch {
	case err != nil || result.Code != 0 || json.Unmarshal([]byte(result.Stdout), &hook) != nil:
		r.fail("%s：SessionStart hook 运行失败：%s", machine, strings.TrimSpace(result.Stdout+" "+result.Stderr))
	case !strings.Contains(hook.Output.Context, "macOS"):
		r.fail("%s：SessionStart hook 没有得到 Mac 的信息：%s", machine, hook.Output.Context)
	default:
		first, _, _ := strings.Cut(hook.Output.Context, "。")
		r.ok("%s：SessionStart hook：%s", machine, first)
	}
	// 家目录里早先手工写的配置已经由系统级设置取代。
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	var settings struct {
		Env map[string]string `json:"env"`
	}
	if data, err := os.ReadFile(settingsPath); err == nil && json.Unmarshal(data, &settings) == nil {
		for _, key := range []string{"CLAUDE_CODE_SHELL", "CLAUDE_CODE_TMPDIR"} {
			if _, ok := settings.Env[key]; ok {
				r.warn("%s：%s 里还有 env.%s，已被系统级设置覆盖，可以删掉", machine, settingsPath, key)
			}
		}
	}
}
