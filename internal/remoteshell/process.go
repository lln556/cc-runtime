package remoteshell

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// 进程组清理的等待时间；测试里可以改小。
var TerminateGrace = 3 * time.Second

// signalGroup 给整个进程组发信号。进程组已经没有成员时返回 false。
func signalGroup(pgid int, sig syscall.Signal) bool {
	err := syscall.Kill(-pgid, sig)
	return err == nil || !errors.Is(err, syscall.ESRCH)
}

// groupAlive 判断进程组里是否还有进程。
func groupAlive(pgid int) bool {
	return signalGroup(pgid, 0)
}

// terminateGroup 先发 SIGTERM，等 TerminateGrace，进程组还在就发 SIGKILL。
// 组长退出后组员仍然属于这个进程组，所以按组发信号就能清理到它们。
func terminateGroup(pgid int) {
	if !signalGroup(pgid, syscall.SIGTERM) {
		return
	}
	deadline := time.Now().Add(TerminateGrace)
	for time.Now().Before(deadline) {
		if !groupAlive(pgid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	signalGroup(pgid, syscall.SIGKILL)
}

// exitStatus 把进程的结束状态换成 shell 的约定：被信号杀死时为 128+信号值。
func exitStatus(cmd *exec.Cmd) Exit {
	state := cmd.ProcessState
	if state == nil {
		return Exit{Code: 255}
	}
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		sig := status.Signal()
		return Exit{Code: 128 + int(sig), Signal: signalName(sig)}
	}
	return Exit{Code: state.ExitCode()}
}

var signalNames = map[syscall.Signal]string{
	syscall.SIGHUP: "HUP", syscall.SIGINT: "INT", syscall.SIGQUIT: "QUIT", syscall.SIGKILL: "KILL",
	syscall.SIGTERM: "TERM", syscall.SIGPIPE: "PIPE", syscall.SIGUSR1: "USR1", syscall.SIGUSR2: "USR2",
	syscall.SIGALRM: "ALRM", syscall.SIGSEGV: "SEGV", syscall.SIGABRT: "ABRT",
}

func signalName(sig syscall.Signal) string {
	if name, ok := signalNames[sig]; ok {
		return name
	}
	return sig.String()
}

// parseSignal 只接受客户端会转发的几种信号。
func parseSignal(name string) (syscall.Signal, bool) {
	switch name {
	case "TERM":
		return syscall.SIGTERM, true
	case "INT":
		return syscall.SIGINT, true
	case "HUP":
		return syscall.SIGHUP, true
	}
	return 0, false
}
