package checks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeke-chin/cc-runtime/internal/testutil"
)

func TestProxyCheckOnRepositoryTemplate(t *testing.T) {
	dir := t.TempDir()
	template, err := os.ReadFile(filepath.Join(testutil.RepoRoot(), "proxy/config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "config.example.yaml"), template, 0o644)
	os.WriteFile(filepath.Join(dir, "config.yaml"), template, 0o644)
	changed := strings.Replace(string(template), "mode: rule\n", "mode: rule\nextra: line\npassword: secret\n", 1)
	os.WriteFile(filepath.Join(dir, "config-other.yaml"), []byte(changed), 0o644)
	results, err := ProxyDifferences(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Name != "config-other.yaml" || results[1].Name != "config.yaml" || len(results[1].Lines) != 0 {
		t.Fatalf("%+v", results)
	}
	text := strings.Join(results[0].Lines, "\n")
	if !strings.Contains(text, "+extra: line") || strings.Contains(text, "secret") || !strings.Contains(text, "<已隐藏凭据行>") {
		t.Fatal(text)
	}
}

func TestUnifiedDiff(t *testing.T) {
	a := strings.Split("1\n2\n3\n4\n5\n6\n7\n8", "\n")
	b := strings.Split("1\n2\nx\n4\n5\n6\n7\n8\n9", "\n")
	got := strings.Join(UnifiedDiff(a, b, "a", "b", 1), "\n")
	want := "--- a\n+++ b\n@@ -2,3 +2,3 @@\n 2\n-3\n+x\n 4\n@@ -8 +8,2 @@\n 8\n+9"
	if got != want {
		t.Fatalf("得到\n%s", got)
	}
	if UnifiedDiff(a, a, "a", "b", 1) != nil {
		t.Fatal("相同时应为空")
	}
}
