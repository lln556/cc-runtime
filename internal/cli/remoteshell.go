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
	machines := remoteshell.Machines(cfg)
	table.Add("开发机", strings.Join(machines, ", "))
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
	machines := remoteshell.Machines(cfg)
	if len(machines) == 0 {
		r.warn("没有启用 remote-shell 的开发机（.env 的 REMOTE_SHELL_MACHINES）")
	}
	project, err := deploy.Model(cfg)
	if err != nil {
		return 0, err
	}
	for _, machine := range machines {
		service, ok := project.Services[machine]
		if !ok {
			r.fail("%s 不是 compose.yaml 里的服务", machine)
			continue
		}
		checkMachine(cfg, r, project, machine, service, port)
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
	if mount := deploy.MountFor(service, filepath.Clean(cfg.Workspace)); mount != nil && filepath.Clean(mount.Source) == filepath.Clean(cfg.LinuxWorkspace()) {
		r.fail("%s：工作区仍挂着 Linux 副本", machine)
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

func checkClaudeConfig(cfg config.Config, r *checkReport, machine, home string) {
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	var settings struct {
		Env map[string]string `json:"env"`
	}
	data, err := os.ReadFile(settingsPath)
	if err == nil {
		err = json.Unmarshal(data, &settings)
	}
	if err != nil {
		r.warn("%s：读不到 %s：%v", machine, settingsPath, err)
	} else {
		want := map[string]string{"CLAUDE_CODE_SHELL": remoteshell.ShellPath, "CLAUDE_CODE_TMPDIR": remoteshell.TmpDir(cfg, machine)}
		for _, key := range []string{"CLAUDE_CODE_SHELL", "CLAUDE_CODE_TMPDIR"} {
			if settings.Env[key] != want[key] {
				r.warn("%s：settings.json 的 env.%s 应为 %s（现在是 %q）", machine, key, want[key], settings.Env[key])
			} else {
				r.ok("%s：settings.json 的 env.%s", machine, key)
			}
		}
	}
	if data, err := os.ReadFile(filepath.Join(home, ".claude", "CLAUDE.md")); err != nil || !strings.Contains(string(data), "remote-shell") {
		r.warn("%s：~/.claude/CLAUDE.md 里没有说明 Bash 在 Mac 上执行（见 docs/remote-shell.md）", machine)
	}
}
