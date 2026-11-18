package remoteshell

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/zeke-chin/cc-runtime/internal/config"
	"github.com/zeke-chin/cc-runtime/internal/jobs"
	"github.com/zeke-chin/cc-runtime/internal/util"
)

const (
	// Label 是 LaunchAgent 的标签。
	Label = jobs.LabelPrefix + "remote-shell"
	// DefaultPort 是服务在 Mac 上监听的端口（只绑定 127.0.0.1），可用 REMOTE_SHELL_PORT 修改。
	DefaultPort = 47100
	// TunnelPort 是网关私网地址上转发到服务的端口（网关配置里的 DIRECT tunnel）。
	TunnelPort = 10024
	// LinuxBinary 是 RUNTIME_DIR/bin 里给开发机用的 cc-runtime（scripts/install.sh 交叉编译）。
	LinuxBinary = "cc-runtime-linux"
)

// Port 是服务监听的端口。
func Port(cfg config.Config) (int, error) {
	value := strings.TrimSpace(cfg.Env["REMOTE_SHELL_PORT"])
	if value == "" {
		return DefaultPort, nil
	}
	port, err := strconv.Atoi(value)
	if err != nil || port <= 0 || port > 65535 {
		return 0, util.Errorf("REMOTE_SHELL_PORT 不是有效的端口：%q", value)
	}
	return port, nil
}

// Machines 是启用 remote-shell 的开发机（.env 的 REMOTE_SHELL_MACHINES，逗号分隔）。
func Machines(cfg config.Config) []string {
	var names []string
	for _, name := range strings.Split(cfg.Env["REMOTE_SHELL_MACHINES"], ",") {
		if name = strings.TrimSpace(name); name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

func Enabled(cfg config.Config, machine string) bool { return slices.Contains(Machines(cfg), machine) }

// MachineDir 是一台开发机的 remote-shell 目录：client.json 和共享的临时目录。
// 它在开发机里挂在相同路径（临时目录要两侧通用），同时挂在 ClientDir（客户端从这里读配置）。
func MachineDir(cfg config.Config, machine string) string {
	return filepath.Join(cfg.Runtime, "remote-shell", machine)
}

func ClientConfigPath(cfg config.Config, machine string) string {
	return filepath.Join(MachineDir(cfg, machine), "client.json")
}

// TmpDir 是给 CLAUDE_CODE_TMPDIR 用的目录：scratchpad 在这里，Mac 上的命令也能看到。
func TmpDir(cfg config.Config, machine string) string {
	return filepath.Join(MachineDir(cfg, machine), "tmp")
}

// BinSource 是挂到开发机 BinDir 的目录。挂目录而不是文件：install.sh 用改名替换二进制，
// 挂单个文件会一直指向旧文件。
func BinSource(cfg config.Config) string { return filepath.Join(cfg.Runtime, "bin") }

func StatePath(cfg config.Config) string { return filepath.Join(cfg.StateDir(), "remote-shell.json") }
func LogPath(cfg config.Config) string   { return filepath.Join(cfg.LogDir(), "remote-shell.log") }

func PlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist")
}

func newToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func readClientConfig(path string) (ClientConfig, error) {
	var cfg ClientConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	return cfg, json.Unmarshal(data, &cfg)
}

// PrepareMachine 为开发机准备 remote-shell 目录和 client.json；已有的 token 沿用。
func PrepareMachine(cfg config.Config, machine, gatewayIP string) (ClientConfig, error) {
	dir := MachineDir(cfg, machine)
	for _, path := range []string{dir, TmpDir(cfg, machine)} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return ClientConfig{}, err
		}
		os.Chmod(path, 0o700)
	}
	if !util.IsDir(BinSource(cfg)) || !util.Lexists(filepath.Join(BinSource(cfg), LinuxBinary)) {
		return ClientConfig{}, util.Errorf("%s 里没有开发机用的 %s；请先在 Mac 上运行 scripts/install.sh", BinSource(cfg), LinuxBinary)
	}
	path := ClientConfigPath(cfg, machine)
	existing, err := readClientConfig(path)
	token := existing.Token
	if err != nil || token == "" {
		if token, err = newToken(); err != nil {
			return ClientConfig{}, err
		}
	}
	port := TunnelPort
	client := ClientConfig{Machine: machine, Addr: net.JoinHostPort(gatewayIP, strconv.Itoa(port)),
		Token: token, TmpDir: TmpDir(cfg, machine), Version: ProtocolVersion}
	if existing == client {
		return client, nil
	}
	data, err := json.MarshalIndent(client, "", "  ")
	if err != nil {
		return ClientConfig{}, err
	}
	// 原地改写（不改名）：开发机挂载的是目录，改名也能看到；保持 0600。
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return ClientConfig{}, err
	}
	os.Chmod(path, 0o600)
	return client, nil
}

// Tokens 读取所有启用的开发机的 token（token → 机器名）。每个请求都会调用：
// 重新读 .env 的机器列表，停用或重新生成 token 即时生效。
func Tokens(cfg config.Config) (map[string]string, error) {
	cfg.Env = config.ReadEnvFile(filepath.Join(cfg.Root, ".env"))
	result := map[string]string{}
	for _, machine := range Machines(cfg) {
		client, err := readClientConfig(ClientConfigPath(cfg, machine))
		if err != nil || client.Token == "" {
			continue
		}
		result[client.Token] = machine
	}
	return result, nil
}

// MachineOverlay 是一台启用 remote-shell 的开发机在 Compose 叠加配置里的挂载：
// 工作区直接挂 Mac 的目录（不再挂 Linux 副本），remote-shell 目录和二进制目录。
func MachineOverlay(cfg config.Config, machine string) []any {
	real := func(path string) string { return util.Resolve(path) }
	bind := func(source, target string, readOnly bool) map[string]any {
		mount := map[string]any{"type": "bind", "source": source, "target": target,
			"bind": map[string]any{"create_host_path": false}}
		if readOnly {
			mount["read_only"] = true
		}
		return mount
	}
	dir := MachineDir(cfg, machine)
	return []any{
		bind(real(cfg.Workspace), filepath.Clean(cfg.Workspace), false),
		bind(real(dir), filepath.Clean(dir), false),
		bind(real(dir), ClientDir, true),
		bind(real(BinSource(cfg)), BinDir, true),
	}
}

// ---- 服务 ----------------------------------------------------------------------

// rotateLog 在日志超过 10 MiB 时把它改名为 .1。
func rotateLog(path string) {
	if info, err := os.Stat(path); err == nil && info.Size() > 10<<20 {
		os.Rename(path, path+".1")
	}
}

// Serve 运行服务，直到收到 SIGTERM/SIGINT。
func Serve(cfg config.Config) error {
	port, err := Port(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.LogDir(), 0o755); err != nil {
		return err
	}
	rotateLog(LogPath(cfg))
	logFile, err := os.OpenFile(LogPath(cfg), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()
	logger := log.New(logFile, "", log.LstdFlags)
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		logger.Printf("无法监听端口 %d：%v", port, err)
		return util.Errorf("无法监听 127.0.0.1:%d：%v", port, err)
	}
	home, _ := os.UserHomeDir()
	cacheDir, _ := os.UserCacheDir()
	snapshot := &EnvSnapshot{Shell: "/bin/zsh", Home: home, Logf: logger.Printf,
		CachePath: filepath.Join(cacheDir, "cc-runtime", "remote-shell", "env.json")}
	server := &Server{
		Shell:      "/bin/zsh",
		SharedDirs: []string{filepath.Clean(cfg.Workspace), filepath.Clean(cfg.Downloads)},
		Env:        snapshot.Get,
		Auth:       func() (map[string]string, error) { return Tokens(cfg) },
		Logf:       logger.Printf,
		State:      StateFile(StatePath(cfg), os.Getpid(), config.Version),
	}
	logger.Printf("启动：版本 %s，监听 127.0.0.1:%d，pid %d", config.Version, port, os.Getpid())
	go snapshot.Get() // 提前生成环境快照，第一条命令不必等

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-stop
		logger.Printf("收到 %v，停止服务", sig)
		listener.Close()
		server.Shutdown("remote-shell：Mac 上的 remote-shell 服务正在重启或停止，命令已中止")
	}()
	err = server.Serve(listener)
	os.Remove(StatePath(cfg))
	logger.Printf("已停止")
	return err
}

// ---- LaunchAgent ---------------------------------------------------------------

func requireMacOS() error {
	if runtime.GOOS != "darwin" {
		return util.Errorf("remote-shell 服务依赖 launchd，只能在 Mac 上管理")
	}
	return nil
}

// Plist 是服务的 LaunchAgent 定义。
func Plist(cfg config.Config) (string, error) {
	binary, err := jobs.Executable()
	if err != nil {
		return "", err
	}
	home, _ := os.UserHomeDir()
	logPath := filepath.Join(cfg.LogDir(), "remote-shell.launchd.log")
	return jobs.RenderPlist([]jobs.KV{
		{Key: "Label", Value: Label},
		{Key: "ProgramArguments", Value: []string{binary, "remote-shell", "serve"}},
		{Key: "EnvironmentVariables", Value: []jobs.KV{{Key: "CC_RUNTIME_ROOT", Value: cfg.Root},
			{Key: "HOME", Value: home}, {Key: "PATH", Value: jobs.LaunchdPath}}},
		{Key: "RunAtLoad", Value: true},
		{Key: "KeepAlive", Value: true},
		// 默认的后台类型会被降低 CPU/IO 优先级，构建会明显变慢。
		{Key: "ProcessType", Value: "Interactive"},
		{Key: "StandardOutPath", Value: logPath},
		{Key: "StandardErrorPath", Value: logPath},
	}), nil
}

func Install(cfg config.Config) error {
	if err := requireMacOS(); err != nil {
		return err
	}
	if _, err := Port(cfg); err != nil {
		return err
	}
	text, err := Plist(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.LogDir(), 0o755); err != nil {
		return err
	}
	path := PlistPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		return err
	}
	util.Run([]string{"launchctl", "bootout", jobs.Domain(), path}, util.Opts{Capture: true, AllowFail: true})
	_, err = util.Run([]string{"launchctl", "bootstrap", jobs.Domain(), path}, util.Opts{Capture: true})
	return err
}

func Uninstall() error {
	if err := requireMacOS(); err != nil {
		return err
	}
	path := PlistPath()
	util.Run([]string{"launchctl", "bootout", jobs.Domain(), path}, util.Opts{Capture: true, AllowFail: true})
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ServiceState 是服务的状态。
type ServiceState struct {
	Installed bool     // plist 存在
	Loaded    bool     // 已加载到 launchd
	Pid       int      // 状态文件里的 pid
	Version   string   // 运行中的服务的版本
	Active    []Active // 进行中的命令
}

func State(cfg config.Config) ServiceState {
	var state ServiceState
	if runtime.GOOS == "darwin" {
		state.Installed = util.Lexists(PlistPath())
		result, err := util.Run([]string{"launchctl", "print", jobs.Domain() + "/" + Label}, util.Opts{Capture: true, AllowFail: true})
		state.Loaded = err == nil && result.Code == 0
	}
	if data, err := os.ReadFile(StatePath(cfg)); err == nil {
		var file struct {
			Pid     int      `json:"pid"`
			Version string   `json:"version"`
			Active  []Active `json:"active"`
		}
		if json.Unmarshal(data, &file) == nil && file.Pid > 0 && syscall.Kill(file.Pid, 0) == nil {
			state.Pid, state.Version, state.Active = file.Pid, file.Version, file.Active
		}
	}
	return state
}

// Restart 重启服务。有进行中的命令时，不加 force 就拒绝（重启会中止它们）。
func Restart(cfg config.Config, force bool) error {
	if err := requireMacOS(); err != nil {
		return err
	}
	state := State(cfg)
	if !state.Loaded {
		return util.Errorf("remote-shell 服务没有加载；用 cc-runtime remote-shell install 安装")
	}
	if len(state.Active) > 0 && !force {
		var lines []string
		for _, a := range state.Active {
			lines = append(lines, fmt.Sprintf("  %s pid=%d：%s", a.Machine, a.Pid, a.Command))
		}
		return util.Errorf("有 %d 条命令正在运行，重启会中止它们：\n%s\n确认要重启就加 --force", len(state.Active), strings.Join(lines, "\n"))
	}
	_, err := util.Run([]string{"launchctl", "kickstart", "-k", jobs.Domain() + "/" + Label}, util.Opts{Capture: true})
	return err
}
