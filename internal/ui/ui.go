// Package ui 负责终端输出：颜色、表格、面板和进度提示。
package ui

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/mattn/go-runewidth"
	"golang.org/x/term"
)

type Style string

const (
	None   Style = ""
	Green  Style = "32"
	Yellow Style = "33"
	Red    Style = "31"
	Blue   Style = "34"
	Dim    Style = "2"
	Bold   Style = "1"
)

var colorOut = os.Getenv("NO_COLOR") == "" && term.IsTerminal(int(os.Stdout.Fd()))
var colorErr = os.Getenv("NO_COLOR") == "" && term.IsTerminal(int(os.Stderr.Fd()))

// SetColor 打开或关闭标准输出的颜色（测试用）。
func SetColor(on bool) { colorOut = on }

func paint(on bool, s Style, text string) string {
	if !on || s == None || text == "" {
		return text
	}
	return "\x1b[" + string(s) + "m" + text + "\x1b[0m"
}

// S 给文本加上样式（输出到标准输出时）。
func (s Style) S(text string) string { return paint(colorOut, s, text) }

// E 给文本加上样式（输出到标准错误时）。
func (s Style) E(text string) string { return paint(colorErr, s, text) }

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// Width 是文本在终端里占的列数（中文算两列，忽略颜色代码）。
func Width(text string) int { return runewidth.StringWidth(ansi.ReplaceAllString(text, "")) }

func pad(text string, width int) string {
	if gap := width - Width(text); gap > 0 {
		return text + strings.Repeat(" ", gap)
	}
	return text
}

// wrap 按显示宽度折行（不在单词边界上找断点，路径和中文都适用）。
func wrap(text string, width int) []string {
	if width <= 0 {
		return []string{text}
	}
	var lines []string
	for _, paragraph := range strings.Split(text, "\n") {
		line, used := "", 0
		for _, r := range paragraph {
			w := runewidth.RuneWidth(r)
			if used+w > width && used > 0 {
				lines = append(lines, line)
				line, used = "", 0
			}
			line += string(r)
			used += w
		}
		lines = append(lines, line)
	}
	return lines
}

// TermWidth 是标准输出的终端宽度；不是终端时为 100。
func TermWidth() int {
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 20 {
		return w
	}
	return 100
}

func Println(text string) { fmt.Println(text) }

func Errln(text string) { fmt.Fprintln(os.Stderr, text) }

// Progress 在标准错误上显示一行提示，返回的函数清除它。不是终端时什么都不做。
func Progress(message string) func() {
	if !term.IsTerminal(int(os.Stderr.Fd())) {
		return func() {}
	}
	fmt.Fprint(os.Stderr, Dim.E(message))
	return func() { fmt.Fprint(os.Stderr, "\r\x1b[K") }
}

// PrintJSON 缩进打印一段 JSON 文本或一个值。
func PrintJSON(value any) {
	var data []byte
	switch v := value.(type) {
	case string:
		var parsed any
		if json.Unmarshal([]byte(v), &parsed) != nil {
			fmt.Println(v)
			return
		}
		value = parsed
	}
	data, _ = MarshalIndent(value)
	fmt.Println(string(data))
}

// MarshalIndent 生成缩进的 JSON，不转义 HTML 字符。
func MarshalIndent(value any) ([]byte, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	err := enc.Encode(value)
	return []byte(strings.TrimRight(b.String(), "\n")), err
}

// Cell 是一个表格单元：文本和样式分开存放，折行后再上色。
type Cell struct {
	Text  string
	Style Style
}

func C(text string, style Style) Cell { return Cell{text, style} }

type Column struct {
	Header string
	Width  int // 0 表示按内容
	Style  Style
}

type Table struct {
	Title   string
	Columns []Column
	Header  bool
	Expand  bool // 表头分隔线占满整个宽度
	Gap     int  // 列间距，0 表示 2
	Rows    [][]Cell
}

func NewTable(title string, header bool, columns ...Column) *Table {
	return &Table{Title: title, Columns: columns, Header: header}
}

// Add 追加一行；参数可以是 string 或 Cell。
func (t *Table) Add(cells ...any) {
	row := make([]Cell, len(t.Columns))
	for i, cell := range cells {
		switch v := cell.(type) {
		case Cell:
			row[i] = v
		case string:
			row[i] = Cell{Text: v}
		default:
			row[i] = Cell{Text: fmt.Sprint(v)}
		}
	}
	t.Rows = append(t.Rows, row)
}

// Lines 按给定宽度排版；最后一列占用剩余宽度并折行。
func (t *Table) Lines(width int) []string {
	gap := 2
	if t.Gap > 0 {
		gap = t.Gap
	}
	widths := make([]int, len(t.Columns))
	for i, col := range t.Columns {
		if col.Width > 0 {
			widths[i] = col.Width
			continue
		}
		if t.Header {
			widths[i] = Width(col.Header)
		}
		for _, row := range t.Rows {
			for _, line := range strings.Split(row[i].Text, "\n") {
				if w := Width(line); w > widths[i] {
					widths[i] = w
				}
			}
		}
	}
	last := len(widths) - 1
	used := gap * last
	for _, w := range widths[:last] {
		used += w
	}
	if last >= 0 && t.Columns[last].Width == 0 && (widths[last] > width-used || width-used < 1) {
		widths[last] = max(width-used, 10)
	}
	total := used
	if last >= 0 {
		total += widths[last]
	}
	if t.Expand {
		total = width
	}
	var lines []string
	if t.Title != "" {
		lines = append(lines, Bold.S(t.Title))
	}
	render := func(cells []Cell, bold bool) {
		wrapped := make([][]string, len(cells))
		height := 1
		for i, cell := range cells {
			wrapped[i] = wrap(cell.Text, widths[i])
			height = max(height, len(wrapped[i]))
		}
		for row := 0; row < height; row++ {
			var parts []string
			for i, cell := range cells {
				text := ""
				if row < len(wrapped[i]) {
					text = wrapped[i][row]
				}
				style := cell.Style
				if style == None {
					style = t.Columns[i].Style
				}
				if bold {
					style = Bold
				}
				if i == last {
					parts = append(parts, style.S(text))
				} else {
					parts = append(parts, style.S(text)+strings.Repeat(" ", max(widths[i]-Width(text), 0)))
				}
			}
			lines = append(lines, strings.TrimRight(strings.Join(parts, strings.Repeat(" ", gap)), " "))
		}
	}
	if t.Header {
		headers := make([]Cell, len(t.Columns))
		for i, col := range t.Columns {
			headers[i] = Cell{Text: col.Header}
		}
		render(headers, true)
		lines = append(lines, strings.Repeat("─", total))
	}
	for _, row := range t.Rows {
		render(row, false)
	}
	return lines
}

func (t *Table) Print() {
	for _, line := range t.Lines(TermWidth()) {
		fmt.Println(line)
	}
}

// Panel 用圆角边框包住若干行，标题在上边，副标题在下边。
func Panel(title, subtitle string, width int, body []string) string {
	inner := width - 4
	border := func(left, label, right string) string {
		if label == "" {
			return Blue.S(left + strings.Repeat("─", width-2) + right)
		}
		label = " " + label + " "
		side := width - 2 - Width(label)
		l := side / 2
		return Blue.S(left+strings.Repeat("─", l)) + label + Blue.S(strings.Repeat("─", side-l)+right)
	}
	var b strings.Builder
	b.WriteString(border("╭", Bold.S(title), "╮") + "\n")
	for _, line := range body {
		b.WriteString(Blue.S("│") + " " + pad(line, inner) + " " + Blue.S("│") + "\n")
	}
	b.WriteString(border("╰", Dim.S(subtitle), "╯"))
	return b.String()
}

// Wrap 按宽度折行（供面板里的普通文本使用）。
func Wrap(text string, width int) []string { return wrap(text, width) }
