package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/zeke-chin/cc-runtime/internal/config"
	"github.com/zeke-chin/cc-runtime/internal/deploy"
	"github.com/zeke-chin/cc-runtime/internal/remoteshell"
	"github.com/zeke-chin/cc-runtime/internal/ui"
)

// 原生安装的 Claude Code 把这三个工具打包在自己的二进制里，在 Bash 里用同名函数接管 grep、find、rg；
// 同一个二进制以 ug 为名时是 ugrep 的简称，原生不接管，这里一并提供。
// remote-shell 服务在 Mac 上定义同样的函数，指向 RUNTIME_DIR/search-tools 里的程序（见 remoteshell.SearchFunctions）。
// 程序从哪来由使用者决定，这里只检查并给出获取方式。
var searchTools = []struct {
	Name, Command, Replaces string
	Optional                bool   // rg、ug：PATH 里已有时不接管，目录里可以没有
	Row                     string // 表格里的行；ug 和 ugrep 是同一个程序，合并在 grep/ug 一行
}{
	{"ugrep", "ugrep", "grep", false, "grep/ug"},
	{"bfs", "bfs", "find", false, "find"},
	{"ripgrep", "rg", "rg", true, "rg"},
	{"ug", "ug", "ug", true, "grep/ug"}, // ugrep 的简称（读 .ugrep 配置），原生不接管，这里一并提供
}

// devClaude 是开发机里 Claude Code 原生安装的入口。
const devClaude = "/home/dev/.local/bin/claude"

// embeddedScript 输出 Claude Code 的版本（`claude\t<路径>\t<版本行>`，没装时路径为空），
// 以及每个内置工具的 `<命令>\t<--version 第一行>`。第一个参数是 claude 的路径。
const embeddedScript = `c="$1"
[ -x "$c" ] || c=$(command -v claude 2>/dev/null) || c=
if [ -z "$c" ]; then printf 'claude\t\t\n'; exit 0; fi
printf 'claude\t%s\t%s\n' "$c" "$("$c" --version 2>/dev/null | head -n 1)"
for t in ugrep bfs rg ug; do
  printf '%s\t%s\n' "$t" "$(ARGV0=$t "$c" --version 2>/dev/null | head -n 1)"
done`

// dirScript 经 remote-shell 在 Mac 上执行（zsh），第一个参数是工具目录。输出两段：
// 目录里每个工具的 `<命令>\t<路径>\t<解析符号链接后的路径>\t<--version 第一行>`（没有时路径为空）；
// 然后 Bash 里 grep、find、rg 实际是什么：`<名字>\t<function|command|none>\t<PATH 里的路径>\t<版本行>`。
const dirScript = `d="$1"
for t in ugrep bfs rg ug; do
  p="$d/$t"
  if [[ -x $p ]]; then
    printf '%s\t%s\t%s\t%s\n' "$t" "$p" "${p:A}" "$("$p" --version 2>/dev/null | head -n 1)"
  else
    printf '%s\t\t\t\n' "$t"
  fi
done
echo __CC_SPLIT__
for n in grep find rg ug; do
  kind=$(whence -w "$n" 2>/dev/null); kind=${kind##*: }
  p=$(whence -p "$n" 2>/dev/null)
  if [[ -n $p ]]; then v=$("$p" --version 2>/dev/null | head -n 1); else v=; fi
  printf '%s\t%s\t%s\t%s\n' "$n" "${kind:-none}" "$p" "$v"
done`

// toolVersion 从 `--version` 的第一行取版本号，例如 "ripgrep 14.1.1 (rev …)" → "14.1.1"。
func toolVersion(line string) string {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return ""
	}
	return fields[1]
}

// parseToolLines 解析脚本输出的制表符分隔行，按第一列索引，并把每行补齐到 width 列（不含第一列）。
func parseToolLines(output string, width int) map[string][]string {
	result := map[string][]string{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) < 2 || fields[0] == "" {
			continue
		}
		rest := fields[1:]
		for len(rest) < width {
			rest = append(rest, "")
		}
		result[fields[0]] = rest
	}
	return result
}

// claudeTools 是开发机里 Claude Code 及其内置工具的版本。
type claudeTools struct {
	Path, Version string
	Tools         map[string]string // 命令 → 版本
}

func parseClaudeTools(output string) claudeTools {
	lines := parseToolLines(output, 2)
	var result claudeTools
	if fields, ok := lines["claude"]; ok {
		result.Path, result.Version = fields[0], strings.TrimSpace(fields[1])
	}
	result.Tools = map[string]string{}
	for _, tool := range searchTools {
		if fields, ok := lines[tool.Command]; ok {
			if version := toolVersion(fields[0]); version != "" {
				result.Tools[tool.Command] = version
			}
		}
	}
	return result
}

func (c claudeTools) describe() string {
	var parts []string
	for _, tool := range searchTools {
		version := c.Tools[tool.Command]
		if version == "" {
			version = "无"
		}
		parts = append(parts, tool.Name+" "+version)
	}
	return fmt.Sprintf("%s 内置 %s", c.Version, strings.Join(parts, "、"))
}

// dirTool 是工具目录里的一个程序。
type dirTool struct{ Path, Target, Version string }

// resolved 是 Bash 在 Mac 上执行某个名字时实际用到的东西。
type resolved struct{ Kind, Path, Version string }

func parseMacTools(output string) (map[string]dirTool, map[string]resolved, bool) {
	before, after, found := strings.Cut(output, "__CC_SPLIT__\n")
	if !found {
		return nil, nil, false
	}
	tools := map[string]dirTool{}
	for name, f := range parseToolLines(before, 3) {
		tools[name] = dirTool{Path: f[0], Target: f[1], Version: toolVersion(f[2])}
	}
	names := map[string]resolved{}
	for name, f := range parseToolLines(after, 3) {
		names[name] = resolved{Kind: f[0], Path: f[1], Version: toolVersion(f[2])}
	}
	return tools, names, true
}

// checkSearchTools 列出开发机里 Claude Code 内置的搜索工具，检查 Mac 上的工具目录里有没有对应的程序，
// 以及 Bash 里的 grep、find、rg 实际用的是什么。只检查，不安装；缺少时提示获取方式，由使用者自己选。
func checkSearchTools(cfg config.Config) (int, error) {
	r := &checkReport{}
	project, err := deploy.Model(cfg)
	if err != nil {
		return 0, err
	}
	machines := deploy.DevServices(project)
	embedded := map[string]map[string]string{} // 命令 → 开发机 → 版本
	var claudes []string
	for _, machine := range machines {
		result, err := deploy.Exec(cfg, machine, "sh", "-c", embeddedScript, "sh", devClaude)
		tools := parseClaudeTools(result.Stdout)
		if err != nil || result.Code != 0 || tools.Path == "" {
			r.fail("%s：读不到开发机里的 Claude Code（退出码 %d）：%s", machine, result.Code, strings.TrimSpace(result.Stderr))
			continue
		}
		for command, version := range tools.Tools {
			if embedded[command] == nil {
				embedded[command] = map[string]string{}
			}
			embedded[command][machine] = version
		}
		claudes = append(claudes, machine+" "+toolVersion("x "+tools.Version))
	}

	dir := remoteshell.SearchToolsDir(cfg)
	tools, names, err := macSearchTools(cfg, machines, dir)
	if err != nil {
		r.fail("%v；先用 cc-runtime check remote-shell 排查", err)
		return exitCode(r.failed), nil
	}

	home, _ := os.UserHomeDir()
	short := func(path string) string {
		if home != "" && strings.HasPrefix(path, home+"/") {
			return "~" + path[len(home):]
		}
		return path
	}
	table := ui.NewTable("", true, ui.Column{Header: "命令"}, ui.Column{Header: "开发机内置"},
		ui.Column{Header: short(dir)}, ui.Column{Header: "Bash 实际用的"})
	table.Gap = 3
	var missing, unreadable []string
	var absent []string
	stale := false
	var rows []string
	type toolCells struct {
		name  string
		cells [3]ui.Cell
	}
	cells := map[string][]toolCells{} // 行 → 每个工具的三格
	for _, tool := range searchTools {
		got, name := tools[tool.Command], names[tool.Replaces]
		want := tool.Name + " " + describeEmbedded(embedded[tool.Command])
		var have ui.Cell
		switch {
		case got.Path == "" && tool.Optional:
			have = ui.C("无", ui.Dim)
		case got.Path == "":
			have = ui.C("缺少", ui.Red)
			missing = append(missing, tool.Command)
		case got.Version == "":
			have = ui.C("读不到版本", ui.Yellow)
			unreadable = append(unreadable, tool.Command)
		default:
			text := tool.Name + " " + got.Version
			if source := toolSource(got, short); source != "" {
				text += "（" + source + "）"
			}
			have = ui.C(text, ui.Green)
		}
		var actual ui.Cell
		switch name.Kind {
		case "function":
			actual = ui.C(tool.Name+"（目录里的）", ui.Green)
		case "command":
			text := short(name.Path)
			if name.Version != "" && name.Version[0] >= '0' && name.Version[0] <= '9' {
				text += " " + name.Version
			}
			actual = ui.C(text, ui.None)
		default:
			actual = ui.C("无", ui.Dim)
			if tool.Optional && got.Path == "" { // 目录里有、只是服务还没加载时算 stale，不算缺少
				absent = append(absent, tool.Command)
			}
		}
		// 目录里有程序，Bash 里的同名命令却没有被函数接管（PATH 里已有 rg、ug 时本来就不接管）：服务还没加载新版本。
		if got.Path != "" && name.Kind != "function" && !(tool.Optional && name.Kind == "command") {
			stale = true
		}
		if cells[tool.Row] == nil {
			rows = append(rows, tool.Row)
		}
		cells[tool.Row] = append(cells[tool.Row], toolCells{tool.Name, [3]ui.Cell{{Text: want}, have, actual}})
	}
	for _, row := range rows {
		group := cells[row]
		merged := group[0].cells
		for _, other := range group[1:] {
			for i := range merged {
				merged[i] = mergeCell(merged[i], other.cells[i], group[0].name, other.name)
			}
		}
		table.Add(row, merged[0], merged[1], merged[2])
	}
	// 和 status 一样包在面板里：宽度按内容，不超过终端。
	natural := 0
	for _, line := range table.Lines(1 << 16) {
		natural = max(natural, ui.Width(line))
	}
	width := min(natural+4, ui.TermWidth())
	fmt.Println(ui.Panel("搜索工具", "开发机 Claude Code "+strings.Join(claudes, "、"), width, table.Lines(width-4)))
	fmt.Println()

	if len(missing) > 0 {
		r.fail("缺少 %s：Bash 里的 %s 用的是 macOS 自带的版本", strings.Join(missing, "、"), replaced(missing))
	}
	if len(unreadable) > 0 {
		r.warn("%s 读不到版本，可能不是对应的工具", strings.Join(unreadable, "、"))
	}
	if len(absent) > 0 {
		r.warn("Bash 里没有 %s：%s 和 PATH 里都没有", strings.Join(absent, "、"), short(dir))
	}
	if stale {
		r.fail("目录里的程序没有生效：remote-shell 服务可能还是旧版本，用 cc-runtime remote-shell restart 重启")
	}
	if len(missing) > 0 || len(absent) > 0 {
		fmt.Printf("\nTips：把程序以 ugrep、bfs、rg、ug 为名放进 %s，来源任选：\n", short(dir))
		fmt.Printf("  - 链接 Mac 上的 Claude Code，和开发机同一套：ln -s ~/.local/bin/claude %s/ugrep（bfs、rg、ug 同理）\n", short(dir))
		fmt.Println("  - Homebrew：brew install ugrep bfs ripgrep（ugrep 自带 ug），再链接过去")
		fmt.Println("  - 自己下载或编译")
	}
	if !r.failed && len(unreadable) == 0 && len(absent) == 0 {
		r.ok("grep、find、rg 和原生 Claude Code 一致，ug 可用")
	}
	return exitCode(r.failed), nil
}

// toolSource 简短说明目录里的程序从哪来：链接到 Claude Code 时写 "Claude Code <版本>"，
// 链接到别处时写目标路径，不是链接时为空。
func toolSource(tool dirTool, short func(string) string) string {
	if tool.Target == "" || tool.Target == tool.Path {
		return ""
	}
	const versions = "/.local/share/claude/versions/"
	if index := strings.Index(tool.Target, versions); index >= 0 {
		return "Claude Code " + tool.Target[index+len(versions):]
	}
	return short(tool.Target)
}

// mergeCell 合并同一行里两个工具的格子。去掉开头的工具名后内容一样（例如 ugrep 和 ug 链接到同一个程序）时
// 只留第一个；否则用 "；" 连起来，第二个前面带上它的名字，并取第二个的颜色（通常是提示问题的颜色）。
func mergeCell(a, b ui.Cell, aName, bName string) ui.Cell {
	if strings.TrimPrefix(a.Text, aName) == strings.TrimPrefix(b.Text, bName) && a.Style == b.Style {
		return a
	}
	text := b.Text
	if !strings.HasPrefix(text, bName) {
		text = bName + " " + text
	}
	style := b.Style
	if style == ui.Green || style == ui.None {
		style = a.Style
	}
	return ui.C(a.Text+"；"+text, style)
}

// replaced 把工具名换成它接管的命令名。
func replaced(commands []string) string {
	var names []string
	for _, command := range commands {
		for _, tool := range searchTools {
			if tool.Command == command {
				names = append(names, tool.Replaces)
			}
		}
	}
	return strings.Join(names, "、")
}

// macSearchTools 经第一台能执行命令的开发机的 remote-shell 查询 Mac，看到的就是 Claude Code 的 Bash 看到的环境。
func macSearchTools(cfg config.Config, machines []string, dir string) (map[string]dirTool, map[string]resolved, error) {
	var last string
	for _, machine := range machines {
		result, err := deploy.Exec(cfg, machine, "sh", "-c", `cd "$1" && exec "$2" -c "$3" zsh "$4"`,
			"sh", cfg.Workspace, remoteshell.ShellPath, dirScript, dir)
		tools, names, ok := parseMacTools(result.Stdout)
		if err != nil || result.Code != 0 || !ok {
			last = fmt.Sprintf("%s：经 remote-shell 查询 Mac 失败（退出码 %d）：%s", machine, result.Code,
				strings.Join(strings.Fields(result.Stdout+" "+result.Stderr), " "))
			continue
		}
		return tools, names, nil
	}
	if last == "" {
		last = "没有开发机可以执行命令"
	}
	return nil, nil, fmt.Errorf("%s", last)
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
