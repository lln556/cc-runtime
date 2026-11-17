package remoteshell

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// RequestTimeout 是连接建立后必须收到请求帧的时间。
var RequestTimeout = 5 * time.Second

// DrainTimeout 是命令结束后等待输出管道关闭的时间。命令留下的后台进程可能一直
// 持有 stdout；超时后照常结束这条命令，不杀它们（和本地 shell 一样）。
var DrainTimeout = time.Second

// Server 接收开发机的命令并在本机执行。
type Server struct {
	Shell string                            // 执行命令的 shell，例如 /bin/zsh
	Env   func() []string                   // 命令的基础环境（Mac 登录 shell 的快照）
	Auth  func() (map[string]string, error) // token → 机器名，每个请求都重新读取
	Logf  func(format string, args ...any)  // 日志
	State func(active []Active)             // 进行中的命令变化时调用（写状态文件）

	mu     sync.Mutex
	nextID int
	active map[int]*running
	closed bool
}

// Active 是一条进行中的命令（写进状态文件）。
type Active struct {
	ID      int       `json:"id"`
	Machine string    `json:"machine"`
	Pid     int       `json:"pid"`
	Cwd     string    `json:"cwd"`
	Command string    `json:"command"`
	Started time.Time `json:"started"`
}

type running struct {
	info   Active
	writer *FrameWriter
	conn   net.Conn
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

func (s *Server) publish() {
	if s.State == nil {
		return
	}
	s.mu.Lock()
	list := make([]Active, 0, len(s.active))
	for _, r := range s.active {
		list = append(list, r.info)
	}
	s.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	s.State(list)
}

// Serve 接受连接，直到 listener 关闭。
func (s *Server) Serve(listener net.Listener) error {
	s.mu.Lock()
	if s.active == nil {
		s.active = map[int]*running{}
	}
	s.mu.Unlock()
	s.publish()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.handle(conn)
	}
}

// Shutdown 中止所有进行中的命令：通知客户端，清理进程组。
func (s *Server) Shutdown(reason string) {
	s.mu.Lock()
	s.closed = true
	list := make([]*running, 0, len(s.active))
	for _, r := range s.active {
		list = append(list, r)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, r := range list {
		wg.Add(1)
		go func(r *running) {
			defer wg.Done()
			r.writer.WriteJSON(FrameFail, Fail{Code: 255, Message: reason})
			terminateGroup(r.info.Pid)
			r.conn.Close()
		}(r)
	}
	wg.Wait()
}

func summarize(command string) string {
	command = strings.Join(strings.Fields(command), " ")
	if index := strings.Index(command, "eval '"); index >= 0 {
		command = command[index+len("eval '"):]
	}
	if len([]rune(command)) > 160 {
		command = string([]rune(command)[:160]) + "…"
	}
	return command
}

func (s *Server) authenticate(token string) (string, error) {
	tokens, err := s.Auth()
	if err != nil {
		return "", err
	}
	machine := ""
	for candidate, name := range tokens {
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(token)) == 1 {
			machine = name
		}
	}
	if machine == "" || token == "" {
		return "", errors.New("token 无效")
	}
	return machine, nil
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	writer := NewFrameWriter(conn)
	fail := func(code int, format string, args ...any) {
		writer.WriteJSON(FrameFail, Fail{Code: code, Message: fmt.Sprintf(format, args...)})
	}
	peer := conn.RemoteAddr().String()

	conn.SetReadDeadline(time.Now().Add(RequestTimeout))
	kind, payload, err := ReadFrame(conn)
	if err != nil || kind != FrameRequest {
		// 连上后什么都不发就断开的是端口探测（check remote-shell），不记日志。
		if !errors.Is(err, io.EOF) {
			s.logf("%s：没有收到请求帧（%v）", peer, err)
		}
		return
	}
	conn.SetReadDeadline(time.Time{})
	var request Request
	if err := json.Unmarshal(payload, &request); err != nil {
		fail(2, "remote-shell：请求格式错误：%v", err)
		return
	}
	if request.Version != ProtocolVersion {
		fail(2, "remote-shell：协议版本不一致（开发机 %d，Mac %d）；请在 Mac 上重新运行 scripts/install.sh 并重建开发机", request.Version, ProtocolVersion)
		return
	}
	machine, err := s.authenticate(request.Token)
	if err != nil {
		s.logf("%s：拒绝：%v", peer, err)
		fail(2, "remote-shell：认证失败：%v", err)
		return
	}
	if info, err := os.Stat(request.Cwd); err != nil || !info.IsDir() {
		fail(1, "remote-shell：工作目录 %s 在 Mac 上不存在。Bash 命令在 Mac 上执行，只能在和 Mac 共享的路径下使用（工作区、下载目录等）", request.Cwd)
		return
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		fail(255, "remote-shell：服务正在重启")
		return
	}
	s.run(conn, writer, machine, request)
}

func (s *Server) run(conn net.Conn, writer *FrameWriter, machine string, request Request) {
	overrides := map[string]string{}
	for key, value := range request.Env {
		overrides[key] = value
	}
	cwdFile := ""
	if request.CaptureCwd {
		file, err := os.CreateTemp("", "remote-shell-cwd-")
		if err == nil {
			cwdFile = file.Name()
			file.Close()
			defer os.Remove(cwdFile)
			overrides[CwdFileVar] = cwdFile
		}
	}
	args := []string{"-c", string(request.Command)}
	for _, arg := range request.Args {
		args = append(args, string(arg))
	}
	cmd := exec.Command(s.Shell, args...)
	cmd.Dir = request.Cwd
	cmd.Env = mergeEnv(s.Env(), overrides)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// 自己建管道：子进程直接拿到文件描述符，Wait 不必等输出读完。
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		writer.WriteJSON(FrameFail, Fail{Code: 255, Message: "remote-shell：" + err.Error()})
		return
	}
	errRead, errWrite, err := os.Pipe()
	if err != nil {
		outRead.Close()
		outWrite.Close()
		writer.WriteJSON(FrameFail, Fail{Code: 255, Message: "remote-shell：" + err.Error()})
		return
	}
	cmd.Stdout, cmd.Stderr = outWrite, errWrite
	started := time.Now()
	err = cmd.Start()
	outWrite.Close()
	errWrite.Close()
	if err != nil {
		outRead.Close()
		errRead.Close()
		writer.WriteJSON(FrameFail, Fail{Code: 127, Message: "remote-shell：无法启动 " + s.Shell + "：" + err.Error()})
		return
	}
	pgid := cmd.Process.Pid

	s.mu.Lock()
	s.nextID++
	id := s.nextID
	entry := &running{info: Active{ID: id, Machine: machine, Pid: pgid, Cwd: request.Cwd,
		Command: summarize(string(request.Command)), Started: started}, writer: writer, conn: conn}
	s.active[id] = entry
	s.mu.Unlock()
	s.publish()
	defer func() {
		s.mu.Lock()
		delete(s.active, id)
		s.mu.Unlock()
		s.publish()
	}()

	// 输出 → 帧。
	var drained sync.WaitGroup
	pump := func(source *os.File, kind byte) {
		defer drained.Done()
		defer source.Close()
		buffer := make([]byte, ChunkSize)
		for {
			n, err := source.Read(buffer)
			if n > 0 {
				if writer.Write(kind, buffer[:n]) != nil {
					// 客户端已经不在；继续读空管道，避免命令因管道写满而卡住，直到被清理。
					io.Copy(io.Discard, source)
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	drained.Add(2)
	go pump(outRead, FrameStdout)
	go pump(errRead, FrameStderr)

	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(exited)
	}()

	// 客户端 → 信号、心跳；连接断开或超时。
	gone := make(chan string, 1)
	go func() {
		for {
			conn.SetReadDeadline(time.Now().Add(PeerTimeout))
			kind, payload, err := ReadFrame(conn)
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					gone <- "心跳超时"
				} else {
					gone <- "连接断开"
				}
				return
			}
			if kind == FrameSignal {
				if sig, ok := parseSignal(string(payload)); ok {
					signalGroup(pgid, sig)
				}
			}
		}
	}()

	ticker := time.NewTicker(PingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			writer.Write(FramePing, nil)
		case reason := <-gone:
			terminateGroup(pgid)
			<-exited
			s.logf("%s pid=%d：%s，已清理进程组（%.1f 秒）：%s", machine, pgid, reason,
				time.Since(started).Seconds(), entry.info.Command)
			return
		case <-exited:
			waitDrained(&drained, DrainTimeout)
			status := exitStatus(cmd)
			if cwdFile != "" {
				if data, err := os.ReadFile(cwdFile); err == nil && len(data) > 0 {
					status.Cwd = data
				}
			}
			writer.WriteJSON(FrameExit, status)
			s.logf("%s pid=%d：退出码 %d（%.1f 秒）cwd=%s：%s", machine, pgid, status.Code,
				time.Since(started).Seconds(), request.Cwd, entry.info.Command)
			// 让客户端先读完再关连接。
			if tcp, ok := conn.(*net.TCPConn); ok {
				tcp.CloseWrite()
			}
			select {
			case <-gone:
			case <-time.After(5 * time.Second):
			}
			return
		}
	}
}

func waitDrained(wg *sync.WaitGroup, timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// StateFile 写出进行中的命令，供 `remote-shell status/restart` 读取。
func StateFile(path string, pid int, version string) func([]Active) {
	return func(active []Active) {
		data, err := json.MarshalIndent(map[string]any{"pid": pid, "version": version,
			"updated": time.Now().Format(time.RFC3339), "active": active}, "", "  ")
		if err != nil {
			return
		}
		os.MkdirAll(filepath.Dir(path), 0o755)
		temporary := path + ".tmp"
		if os.WriteFile(temporary, data, 0o644) == nil {
			os.Rename(temporary, path)
		}
	}
}
