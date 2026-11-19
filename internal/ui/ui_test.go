package ui

import (
	"strings"
	"testing"
)

func TestTableAlignsWideCharactersAndWrapsLastColumn(t *testing.T) {
	SetColor(false)
	table := NewTable("标题", true, Column{Header: "项目", Width: 6}, Column{Header: "状态"})
	table.Add("冲突", "0")
	table.Add("说明", strings.Repeat("长", 12))
	lines := table.Lines(20)
	want := []string{"标题", "项目    状态", "────────────────────", "冲突    0",
		"说明    长长长长长长", "        长长长长长长"}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("得到\n%s", strings.Join(lines, "\n"))
	}
	for _, line := range lines {
		if Width(line) > 20 {
			t.Errorf("超宽：%q", line)
		}
	}
}

func TestPanelKeepsWidth(t *testing.T) {
	SetColor(false)
	for _, line := range strings.Split(Panel("标题", "副标题", 30, []string{"中文内容", "abc"}), "\n") {
		if Width(line) != 30 {
			t.Errorf("宽度 %d：%q", Width(line), line)
		}
	}
}

func TestTableGap(t *testing.T) {
	SetColor(false)
	table := NewTable("", false, Column{}, Column{})
	table.Add("a", "b")
	if got := table.Lines(80)[0]; got != "a  b" {
		t.Fatalf("默认间距：%q", got)
	}
	table.Gap = 4
	if got := table.Lines(80)[0]; got != "a    b" {
		t.Fatalf("Gap=4：%q", got)
	}
}
