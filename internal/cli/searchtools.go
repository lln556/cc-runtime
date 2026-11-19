package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/zeke-chin/cc-runtime/internal/config"
	"github.com/zeke-chin/cc-runtime/internal/deploy"
	"github.com/zeke-chin/cc-runtime/internal/remoteshell"
)

// 原生安装的 Claude Code 把这三个工具打包在自己的二进制里，在 Bash 里用同名函数接管 grep、find、rg。
// 开发机的 Bash 命令在 Mac 上执行，用不到开发机里的二进制，这些工具要在 Mac 上另外安装。
var searchTools = []struct{ Name, Command, Replaces string }{
	{"ugrep", "ugrep", "grep"},
	{"bfs", "bfs", "find"},
	{"ripgrep", "rg", "rg"},
}

// embeddedScript 在开发机里输出 Claude Code 的版本，以及每个内置工具的 `<命令>\t<--version 第一行>`。
const embeddedScript = `c="$HOME/.local/bin/claude"
[ -x "$c" ] || c=$(command -v claude) || exit 3
"$c" --version 2>/dev/null | head -n 1
for t in ugrep bfs rg; do
  printf '%s\t%s\n' "$t" "$(ARGV0=$t "$c" --version 2>/dev/null | head -n 1)"
done`

// macScript 经 remote-shell 在 Mac 上执行，输出 `<命令>\t<路径>\t<--version 第一行>`；没装时路径为空。
// 用 whence -p 只找 PATH 里的程序，不受同名函数和别名影响。
const macScript = `for t in ugrep bfs rg; do
  p=$(whence -p "$t" 2>/dev/null)
  if [ -n "$p" ]; then v=$("$p" --version 2>/dev/null | head -n 1); else v=; fi
  printf '%s\t%s\t%s\n' "$t" "$p" "$v"
done`

// toolVersion 从 `--version` 的第一行取版本号，例如 "ripgrep 14.1.1 (rev …)" → "14.1.1"。
func toolVersion(line string) string {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return ""
	}
	return fields[1]
}

// parseToolLines 解析脚本输出的制表符分隔行，按第一列（命令名）索引。
func parseToolLines(output string) map[string][]string {
	result := map[string][]string{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) >= 2 && fields[0] != "" {
			result[fields[0]] = fields[1:]
		}
	}
	return result
}

type macTool struct{ Path, Version string }

// checkSearchTools 列出开发机里 Claude Code 内置的搜索工具版本，检查 Mac 上有没有装；只检查，不安装。
func checkSearchTools(cfg config.Config) (int, error) {
	r := &checkReport{}
	project, err := deploy.Model(cfg)
	if err != nil {
		return 0, err
	}
	machines := deploy.DevServices(project)
	embedded := map[string]map[string]string{} // 命令 → 开发机 → 版本
	for _, machine := range machines {
		result, err := deploy.Exec(cfg, machine, "sh", "-c", embeddedScript)
		lines := strings.SplitN(result.Stdout, "\n", 2)
		if err != nil || result.Code != 0 || len(lines) < 2 {
			r.fail("%s：读不到开发机里 Claude Code 的版本（退出码 %d）：%s", machine, result.Code,
				strings.TrimSpace(result.Stderr))
			continue
		}
		tools := parseToolLines(lines[1])
		var parts []string
		for _, tool := range searchTools {
			version := ""
			if fields := tools[tool.Command]; len(fields) > 0 {
				version = toolVersion(fields[0])
			}
			if version == "" {
				parts = append(parts, tool.Name+" 无")
				continue
			}
			if embedded[tool.Command] == nil {
				embedded[tool.Command] = map[string]string{}
			}
			embedded[tool.Command][machine] = version
			parts = append(parts, tool.Name+" "+version)
		}
		r.ok("%s：%s 内置 %s", machine, strings.TrimSpace(lines[0]), strings.Join(parts, "、"))
	}

	mac, err := macSearchTools(cfg, machines)
	if err != nil {
		r.fail("%v；先用 cc-runtime check remote-shell 排查", err)
		return exitCode(r.failed), nil
	}
	for _, tool := range searchTools {
		want := describeEmbedded(embedded[tool.Command])
		got := mac[tool.Command]
		switch {
		case got.Path == "":
			r.fail("%s（替代 %s）：Mac 上没有安装，开发机内置的是 %s；请在 Mac 上自行安装", tool.Name, tool.Replaces, want)
		case got.Version == "":
			r.warn("%s：Mac 上的 %s 读不到版本，开发机内置的是 %s", tool.Name, got.Path, want)
		default:
			r.ok("%s：Mac 上 %s（%s），开发机内置 %s", tool.Name, got.Version, got.Path, want)
		}
	}
	return exitCode(r.failed), nil
}

// macSearchTools 经第一台能执行命令的开发机的 remote-shell 查询 Mac 上的工具，看到的就是 Claude Code 的 Bash 看到的环境。
func macSearchTools(cfg config.Config, machines []string) (map[string]macTool, error) {
	var last string
	for _, machine := range machines {
		result, err := deploy.Exec(cfg, machine, "sh", "-c", `cd "$1" && exec "$2" -c "$3"`,
			"sh", cfg.Workspace, remoteshell.ShellPath, macScript)
		if err != nil || result.Code != 0 {
			last = fmt.Sprintf("%s：经 remote-shell 查询 Mac 上的工具失败（退出码 %d）：%s", machine, result.Code,
				strings.Join(strings.Fields(result.Stdout+" "+result.Stderr), " "))
			continue
		}
		tools := map[string]macTool{}
		for name, fields := range parseToolLines(result.Stdout) {
			tool := macTool{Path: fields[0]}
			if len(fields) > 1 {
				tool.Version = toolVersion(fields[1])
			}
			tools[name] = tool
		}
		return tools, nil
	}
	if last == "" {
		last = "没有开发机可以执行命令"
	}
	return nil, fmt.Errorf("%s", last)
}

// describeEmbedded 把各开发机的版本合并成一句：都一样时只写版本号，不一样时逐台列出。
func describeEmbedded(versions map[string]string) string {
	if len(versions) == 0 {
		return "未知"
	}
	distinct := map[string]bool{}
	var machines []string
	for machine, version := range versions {
		distinct[version] = true
		machines = append(machines, machine)
	}
	sort.Strings(machines)
	if len(distinct) == 1 {
		return versions[machines[0]]
	}
	var parts []string
	for _, machine := range machines {
		parts = append(parts, machine+" "+versions[machine])
	}
	return strings.Join(parts, "、")
}
