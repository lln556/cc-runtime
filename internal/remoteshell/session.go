package remoteshell

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// SessionStartName 是 SessionStart hook 的入口名：BinDir/remote-shell/session-start 链接到
// 同一个程序，按文件名分派。
const SessionStartName = "session-start"

// SessionStartPath 是 hook 在开发机里的路径（/etc/claude-code/managed-settings.json 引用它）。
const SessionStartPath = BinDir + "/remote-shell/" + SessionStartName

// ProbeCommand 在 Mac 上取系统版本和架构，每行一项；测试里替换。
var ProbeCommand = `printf '%s\n' "$(sw_vers -productVersion)" "$(sw_vers -buildVersion)" "$(uname -m)"`

// ProbeTimeout 是探测的总时限：hook 不能拖住会话开始。
var ProbeTimeout = 5 * time.Second

type hookInput struct {
	Cwd    string `json:"cwd"`
	Source string `json:"source"`
}

// SessionStartMain 是 Claude Code 的 SessionStart hook：在会话的工作目录里经 remote-shell
// 探测一次 Mac，把执行环境写进上下文。系统提示里的 Platform、OS Version 描述的是容器，
// 这里纠正它；工作目录在 Mac 上不存在或服务不可达时，在会话一开始就说明。总是返回 0。
func SessionStartMain(stdin io.Reader, stdout io.Writer) int {
	var input hookInput
	if data, err := io.ReadAll(io.LimitReader(stdin, 1<<20)); err == nil {
		json.Unmarshal(data, &input)
	}
	if input.Cwd == "" {
		input.Cwd, _ = os.Getwd()
	}
	output := map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "SessionStart", "additionalContext": sessionContext(input.Cwd)}}
	data, _ := json.Marshal(output)
	stdout.Write(data)
	return 0
}

func sessionContext(cwd string) string {
	if os.Getenv(LocalSwitch) == "1" {
		return LocalSwitch + "=1：本会话的 Bash 命令在开发机容器（Linux）里执行，不在 Mac 上。"
	}
	cfg, err := loadClientConfig()
	if err != nil {
		return "警告：remote-shell 的配置不可用（" + err.Error() + "）。本会话里的 Bash 命令会失败，" +
			"不会改在容器里执行；先把这一点告诉使用者。"
	}
	code, stdout, stderr := probe(cfg, cwd)
	switch {
	case code == 0:
		return describe(cfg, strings.Fields(stdout))
	case code == 255:
		return "警告：remote-shell 服务不可达：" + stderr + "\n本会话里的 Bash 命令会失败，不会改在容器里执行；" +
			"先告诉使用者在 Mac 上检查服务（cc-runtime remote-shell status）。"
	default:
		return "警告：" + stderr + "\n在使用者换到共享目录重新启动之前，本会话里的 Bash 命令都会被拒绝；先把上面的说明转告使用者。"
	}
}

// probe 在 cwd 里执行 ProbeCommand，超时按服务不可达处理。
func probe(cfg ClientConfig, cwd string) (int, string, string) {
	type result struct {
		code           int
		stdout, stderr string
	}
	done := make(chan result, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		request := Request{Version: ProtocolVersion, Token: cfg.Token, Cwd: cwd, Command: []byte(ProbeCommand)}
		code := run(cfg.Addr, request, "", &stdout, &stderr)
		done <- result{code, stdout.String(), strings.TrimSpace(stderr.String())}
	}()
	select {
	case r := <-done:
		return r.code, r.stdout, r.stderr
	case <-time.After(ProbeTimeout):
		return 255, "", fmt.Sprintf("%s 内没有回应（%s）", ProbeTimeout, cfg.Addr)
	}
}

func describe(cfg ClientConfig, facts []string) string {
	for len(facts) < 3 {
		facts = append(facts, "?")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "本会话的 Bash 命令经 remote-shell 在宿主 Mac 上执行：macOS %s（%s），%s，zsh。"+
		"系统信息里的 Platform、OS Version 描述的是运行 Claude Code 的 Linux 容器，不是命令执行的地方。", facts[0], facts[1], facts[2])
	if len(cfg.SharedDirs) > 0 {
		fmt.Fprintf(&b, "\n容器和 Mac 共享（路径相同）的目录：%s。", strings.Join(cfg.SharedDirs, "、"))
	}
	if cfg.TmpDir != "" {
		fmt.Fprintf(&b, "\nClaude Code 的临时目录（scratchpad 在其中）：%s，两侧路径相同。", cfg.TmpDir)
	}
	if cfg.Container != "" {
		fmt.Fprintf(&b, "\n运行 Claude Code 的容器：%s；需要容器内的环境时，在 Bash 里用 docker exec %s sh -c '…'。",
			cfg.Container, cfg.Container)
	}
	return b.String()
}
