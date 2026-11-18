// Package checks 是一致性检查：网关配置与模板是否一致。
package checks

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ---- 网关配置 ----------------------------------------------------------

var (
	block   = regexp.MustCompile(`(?s)[ ]*# ==== GATEWAY-SPECIFIC.*?# ==== END GATEWAY-SPECIFIC ====\n`)
	airport = regexp.MustCompile(`(?s)  - name: airport\n.*?\n\n`)
	// 模板里的占位 IP，后面紧跟本地局域网注释。
	placeholderIPs = regexp.MustCompile(`IP-CIDR,[0-9.]+/32,DIRECT,no-resolve\n(  # Local LAN subnet)`)
	credential     = regexp.MustCompile(`(password|username|uuid|public-key|short-id|cipher)\s*:`)
)

func NormalizeProxy(text string) (string, error) {
	start := strings.Index(text, "mode: rule\n")
	if start < 0 {
		return "", fmt.Errorf("找不到 \"mode: rule\"")
	}
	text = text[start:]
	text = block.ReplaceAllString(text, "<GATEWAY-SPECIFIC>\n")
	text = airport.ReplaceAllString(text, "<AIRPORT>\n\n")
	return placeholderIPs.ReplaceAllString(text, "<DIRECT-HOST>\n$1"), nil
}

type ProxyResult struct {
	Name  string
	Lines []string
}

// ProxyDifferences 对每个 config*.yaml（模板除外）给出它与模板的差异。
//
// 在 GATEWAY-SPECIFIC 块之外，每份实际配置都必须与 config.example.yaml 相同，
// 机场节点和文档里说明的占位符除外。像凭据的行从不显示。
func ProxyDifferences(proxyDir string) ([]ProxyResult, error) {
	template, err := os.ReadFile(filepath.Join(proxyDir, "config.example.yaml"))
	if err != nil {
		return nil, err
	}
	normalized, err := NormalizeProxy(string(template))
	if err != nil {
		return nil, fmt.Errorf("config.example.yaml：%w", err)
	}
	expected := strings.Split(normalized, "\n")
	paths, _ := filepath.Glob(filepath.Join(proxyDir, "config*.yaml"))
	sort.Strings(paths)
	var results []ProxyResult
	for _, path := range paths {
		name := filepath.Base(path)
		if name == "config.example.yaml" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		text, err := NormalizeProxy(string(data))
		if err != nil {
			return nil, fmt.Errorf("%s：%w", name, err)
		}
		var lines []string
		for _, line := range UnifiedDiff(expected, strings.Split(text, "\n"), "config.example.yaml", name, 1) {
			if credential.MatchString(line) {
				line = line[:1] + "  <已隐藏凭据行>"
			}
			lines = append(lines, line)
		}
		results = append(results, ProxyResult{name, lines})
	}
	return results, nil
}

// UnifiedDiff 生成 unified diff 格式的差异（相同时为空）。
func UnifiedDiff(a, b []string, fromName, toName string, context int) []string {
	// 最长公共子序列；配置文件只有几百行，O(n·m) 足够。
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	type op struct {
		kind byte // ' '、'-'、'+'
		i, j int  // 在 a、b 中的位置（下一行）
		text string
	}
	var ops []op
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && a[i] == b[j]:
			ops = append(ops, op{' ', i, j, a[i]})
			i, j = i+1, j+1
		case i < n && (j == m || lcs[i+1][j] >= lcs[i][j+1]):
			ops = append(ops, op{'-', i, j, a[i]})
			i++
		default:
			ops = append(ops, op{'+', i, j, b[j]})
			j++
		}
	}
	var out []string
	for k := 0; k < len(ops); {
		if ops[k].kind == ' ' {
			k++
			continue
		}
		start := max(k-context, 0)
		end := k
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			run := end
			for run < len(ops) && ops[run].kind == ' ' {
				run++
			}
			if run == len(ops) || run-end > 2*context {
				end = min(end+context, len(ops))
				break
			}
			end = run
		}
		if len(out) == 0 {
			out = append(out, "--- "+fromName, "+++ "+toName)
		}
		var aCount, bCount int
		for _, o := range ops[start:end] {
			if o.kind != '+' {
				aCount++
			}
			if o.kind != '-' {
				bCount++
			}
		}
		out = append(out, fmt.Sprintf("@@ -%s +%s @@", hunkRange(ops[start].i, aCount), hunkRange(ops[start].j, bCount)))
		for _, o := range ops[start:end] {
			out = append(out, string(o.kind)+o.text)
		}
		k = end
	}
	return out
}

func hunkRange(start, count int) string {
	if count == 1 {
		return fmt.Sprint(start + 1)
	}
	if count == 0 {
		return fmt.Sprintf("%d,0", start)
	}
	return fmt.Sprintf("%d,%d", start+1, count)
}
