package remoteshell

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// 开发机里的固定路径。
const (
	// ClientDir 是本机 remote-shell 目录在开发机里的固定挂载点，里面有 client.json。
	ClientDir = "/var/lib/cc-runtime/remote-shell"
	// BinDir 是 RUNTIME_DIR/bin 在开发机里的挂载点。放在 bin 子目录，不遮住镜像里
	// /opt/cc-runtime 下的 shell 配置。
	BinDir = "/opt/cc-runtime/bin"
	// ShellPath 是给 CLAUDE_CODE_SHELL 用的路径。Claude Code 只接受路径里含 bash 或 zsh 的 shell，
	// 所以名字放在目录上。
	ShellPath = BinDir + "/remote-shell/zsh"
	// LocalSwitch 为 1 时客户端直接在开发机本地执行（临时回到 Linux）。
	LocalSwitch = "REMOTE_SHELL_LOCAL"
	// ConfigEnv 可以指定 client.json 的位置（测试用）。
	ConfigEnv = "REMOTE_SHELL_CONFIG"
)

// ClientConfig 是 `cc-runtime up` 为每台开发机生成的客户端配置。
type ClientConfig struct {
	Machine string `json:"machine"`
	Addr    string `json:"addr"`    // 网关上的 tunnel 地址
	Token   string `json:"token"`   // 每台开发机一个
	TmpDir  string `json:"tmpDir"`  // 给 CLAUDE_CODE_TMPDIR 用的共享目录
	Version int    `json:"version"` // 协议版本
}

// 客户端等待服务回应信号的时间；测试里可以改小。
var SignalGrace = 3 * time.Second

// DialTimeout 是连接服务的超时。
var DialTimeout = 5 * time.Second

func loadClientConfig() (ClientConfig, error) {
	path := os.Getenv(ConfigEnv)
	if path == "" {
		path = filepath.Join(ClientDir, "client.json")
	}
	var cfg ClientConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("读不到 %s：%w；在 Mac 上用 cc-runtime up 重建这台开发机", path, err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("%s 格式错误：%w", path, err)
	}
	if cfg.Addr == "" || cfg.Token == "" {
		return cfg, fmt.Errorf("%s 缺少 addr 或 token", path)
	}
	return cfg, nil
}

// localShell 是临时回到开发机本地执行时用的 shell。
func localShell() string {
	for _, candidate := range []string{"/usr/bin/zsh", "/bin/zsh", "/bin/bash"} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return "/bin/sh"
}

func errorf(stderr io.Writer, format string, args ...any) {
	fmt.Fprintf(stderr, "remote-shell："+format+"\n", args...)
}

// ClientMain 是开发机里 remote-shell 客户端的入口，参数和 shell 相同，返回退出码。
func ClientMain(args []string) int {
	if os.Getenv(LocalSwitch) == "1" {
		shell := localShell()
		err := syscall.Exec(shell, append([]string{filepath.Base(shell)}, args...), os.Environ())
		errorf(os.Stderr, "无法在本地执行 %s：%v", shell, err)
		return 127
	}
	invocation, err := ParseArgs(args)
	if err != nil {
		errorf(os.Stderr, "%v", err)
		return 2
	}
	// Claude Code 启动时创建 shell 快照：快照会在命令前被 source，而命令在 Mac 上执行，
	// 开发机里的快照没有意义。直接写一个空快照，Mac 的环境由服务提供。
	if path, ok := SnapshotFile(invocation.Command); ok {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
			err = os.WriteFile(path, []byte(SnapshotContent), 0o644)
		}
		if err != nil {
			errorf(os.Stderr, "无法写快照 %s：%v", path, err)
			return 1
		}
		return 0
	}
	cfg, err := loadClientConfig()
	if err != nil {
		errorf(os.Stderr, "%v", err)
		return 255
	}
	cwd, err := os.Getwd()
	if err != nil {
		errorf(os.Stderr, "无法取得当前目录：%v", err)
		return 255
	}
	command, cwdFile, rewritten := RewriteCwd(invocation.Command)
	request := Request{Version: ProtocolVersion, Token: cfg.Token, Cwd: cwd, Command: []byte(command),
		Env: FilterEnv(os.Environ()), CaptureCwd: rewritten}
	for _, arg := range invocation.Args {
		request.Args = append(request.Args, []byte(arg))
	}
	return run(cfg.Addr, request, cwdFile, os.Stdout, os.Stderr)
}

type event struct {
	kind    byte
	payload []byte
	err     error
}

// run 把请求发给服务，转发输出、信号和心跳，返回命令的退出码。
func run(addr string, request Request, cwdFile string, stdout, stderr io.Writer) int {
	// 先注册信号：连接期间收到的中断也要转发。
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(signals)

	conn, err := net.DialTimeout("tcp", addr, DialTimeout)
	if err != nil {
		errorf(stderr, "无法连接 Mac 上的 remote-shell 服务（%s）：%v", addr, err)
		return 255
	}
	defer conn.Close()
	writer := NewFrameWriter(conn)
	if err := writer.WriteJSON(FrameRequest, request); err != nil {
		errorf(stderr, "发送请求失败：%v", err)
		return 255
	}

	events := make(chan event, 16)
	go func() {
		for {
			kind, payload, err := ReadFrame(conn)
			events <- event{kind, payload, err}
			if err != nil || kind == FrameExit || kind == FrameFail {
				return
			}
		}
	}()

	ticker := time.NewTicker(PingInterval)
	defer ticker.Stop()
	lastSeen := time.Now()
	var pending syscall.Signal
	var deadline <-chan time.Time
	for {
		select {
		case ev := <-events:
			if ev.err != nil {
				if pending != 0 {
					return 128 + int(pending)
				}
				errorf(stderr, "与 Mac 的连接中断：%v", ev.err)
				return 255
			}
			lastSeen = time.Now()
			switch ev.kind {
			case FrameStdout:
				stdout.Write(ev.payload)
			case FrameStderr:
				stderr.Write(ev.payload)
			case FrameExit:
				var exit Exit
				if err := json.Unmarshal(ev.payload, &exit); err != nil {
					errorf(stderr, "退出帧格式错误：%v", err)
					return 255
				}
				if cwdFile != "" && len(exit.Cwd) > 0 {
					if err := os.WriteFile(cwdFile, exit.Cwd, 0o644); err != nil {
						errorf(stderr, "无法写回工作目录：%v", err)
					}
				}
				return exit.Code
			case FrameFail:
				var fail Fail
				if err := json.Unmarshal(ev.payload, &fail); err != nil {
					errorf(stderr, "失败帧格式错误：%v", err)
					return 255
				}
				fmt.Fprintln(stderr, fail.Message)
				if fail.Code == 0 {
					return 255
				}
				return fail.Code
			}
		case sig := <-signals:
			s := sig.(syscall.Signal)
			writer.Write(FrameSignal, []byte(signalName(s)))
			if pending == 0 {
				pending = s
				deadline = time.After(SignalGrace)
			}
		case <-deadline:
			// 服务没有及时结束命令：关闭连接，由服务清理整个进程组。
			return 128 + int(pending)
		case <-ticker.C:
			if time.Since(lastSeen) > PeerTimeout {
				errorf(stderr, "Mac 上的 remote-shell 服务无响应（%s 没有心跳）", PeerTimeout)
				return 255
			}
			writer.Write(FramePing, nil)
		}
	}
}
