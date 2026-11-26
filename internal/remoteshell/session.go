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

type sessionAgent struct {
	name        string
	commandName string
	inTool      string
	systemField string
	tmpDesc     string
	cli         string
}

var (
	claudeAgent = sessionAgent{
		name:        "Claude Code",
		commandName: "Bash 命令",
		inTool:      "在 Bash 里",
		systemField: "Platform、OS Version",
		tmpDesc:     "Claude Code 的临时目录（scratchpad 在其中）",
		cli:         "claude",
	}
	codexAgent = sessionAgent{
		name:        "Codex",
		commandName: "shell 命令",
		inTool:      "在 shell 工具里",
		systemField: "<environment_context>",
		tmpDesc:     "remote-shell 共享临时目录",
		cli:         "codex",
	}
)

// SessionStartMain 是 Claude Code 的 SessionStart hook：在会话的工作目录里经 remote-shell
// 探测一次 Mac，把执行环境写进上下文。系统提示里的 Platform、OS Version 描述的是容器，
// 这里纠正它；工作目录在 Mac 上不存在或服务不可达时，在会话一开始就说明。总是返回 0。
func SessionStartMain(stdin io.Reader, stdout io.Writer) int {
	return runSessionStart(stdin, stdout, claudeAgent)
}

func runSessionStart(stdin io.Reader, stdout io.Writer, agent sessionAgent) int {
	var input hookInput
	if data, err := io.ReadAll(io.LimitReader(stdin, 1<<20)); err == nil {
		json.Unmarshal(data, &input)
	}
	cwd := input.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	cwd = evalSymlinksOrKeep(cwd)
	output := map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "SessionStart", "additionalContext": sessionContextFor(cwd, agent)}}
	data, _ := json.Marshal(output)
	stdout.Write(data)
	return 0
}

func sessionContext(cwd string) string {
	return sessionContextFor(cwd, claudeAgent)
}

func sessionContextFor(cwd string, agent sessionAgent) string {
	if os.Getenv(LocalSwitch) == "1" {
		return LocalSwitch + "=1：本会话的 " + agent.commandName + "在开发机容器（Linux）里执行，不在 Mac 上。"
	}
	cfg, err := loadClientConfig()
	if err != nil {
		return "警告：remote-shell 的配置不可用（" + err.Error() + "）。本会话里的 " + agent.commandName + "会失败，" +
			"不会改在容器里执行；先把这一点告诉使用者。"
	}
	code, stdout, stderr := probe(cfg, cwd)
	switch {
	case code == 0:
		return describeFor(cfg, strings.Fields(stdout), agent)
	case code == 255:
		return "警告：remote-shell 服务不可达：" + stderr + "\n本会话里的 " + agent.commandName + "会失败，不会改在容器里执行；" +
			"先告诉使用者在 Mac 上检查服务（cc-runtime remote-shell status）。"
	default:
		if agent.name != claudeAgent.name {
			stderr = strings.Replace(stderr, "Bash 命令在宿主 Mac 上执行", agent.commandName+"在宿主 Mac 上执行", 1)
			stderr = strings.Replace(stderr, "重新启动 Claude Code", "重新启动 "+agent.name, 1)
			stderr = strings.Replace(stderr, " && claude\n", " && "+agent.cli+"\n", 1)
			if strings.HasSuffix(stderr, " && claude") {
				stderr = strings.TrimSuffix(stderr, " && claude") + " && " + agent.cli
			}
		}
		return "警告：" + stderr + "\n在使用者换到共享目录重新启动之前，本会话里的 " + agent.commandName + "都会被拒绝；先把上面的说明转告使用者。"
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
	return describeFor(cfg, facts, claudeAgent)
}

func describeFor(cfg ClientConfig, facts []string, agent sessionAgent) string {
	for len(facts) < 3 {
		facts = append(facts, "?")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "本会话的 %s经 remote-shell 在宿主 Mac 上执行：macOS %s（%s），%s，zsh。"+
		"系统信息里的 %s 描述的是运行 %s 的 Linux 容器，不是命令执行的地方。",
		agent.commandName, facts[0], facts[1], facts[2], agent.systemField, agent.name)
	if len(cfg.SharedDirs) > 0 {
		fmt.Fprintf(&b, "\n容器和 Mac 共享（路径相同）的目录：%s。", strings.Join(cfg.SharedDirs, "、"))
	}
	if cfg.TmpDir != "" {
		fmt.Fprintf(&b, "\n%s：%s，两侧路径相同。", agent.tmpDesc, cfg.TmpDir)
	}
	if cfg.Container != "" {
		fmt.Fprintf(&b, "\n运行 %s 的容器：%s；需要容器内的环境时，%s用 docker exec %s sh -c '…'。",
			agent.name, cfg.Container, agent.inTool, cfg.Container)
	}
	return b.String()
}
