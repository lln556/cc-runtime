package deploy

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/zeke-chin/cc-runtime/internal/config"
	"github.com/zeke-chin/cc-runtime/internal/testutil"
	"github.com/zeke-chin/cc-runtime/internal/util"
)

var dockerTests = os.Getenv("CC_RUNTIME_DOCKER_TEST") == "1"

func testImage() string {
	if image := os.Getenv("CC_RUNTIME_TEST_IMAGE"); image != "" {
		return image
	}
	return "ghcr.io/zeke-chin/cc-runtime:latest"
}

func machine(base, name string) Service {
	address := "172.30.0.11"
	if name == "dev-pennsylvania" {
		address = "172.30.0.10"
	}
	return Service{Image: testImage(), Volumes: []Mount{
		{Type: "bind", Source: filepath.Join(base, "dev-runtimes", name, "ssh-host-keys"), Target: SSHTarget},
		{Type: "bind", Source: filepath.Join(base, "dev-runtimes", name, "machine-id"), Target: "/etc/machine-id"},
		{Type: "bind", Source: filepath.Join(base, "dev-runtimes", name, "home"), Target: HomeTarget},
	}, Networks: map[string]*Network{"private": {IPv4Address: address}}}
}

func project(root string) Project {
	return Project{Name: "cc-runtime", Services: map[string]Service{
		"dev-pennsylvania": machine(root, "dev-pennsylvania"), "dev-california": machine(root, "dev-california"),
		"dev-pennsylvania-network-init": {Image: "x"}, "proxy-pennsylvania": {Image: "x"}}}
}

func TestDevServicesAndHelpers(t *testing.T) {
	p := project(t.TempDir())
	if got := DevServices(p); !slices.Equal(got, []string{"dev-california", "dev-pennsylvania"}) {
		t.Fatal(got)
	}
	if got := WithHelpers(p, []string{"dev-pennsylvania", "proxy-pennsylvania"}); !slices.Equal(got, []string{"dev-pennsylvania", "proxy-pennsylvania", "dev-pennsylvania-network-init"}) {
		t.Fatal(got)
	}
}

func TestAddressConflictsWithRunningOrphans(t *testing.T) {
	var orphan Container
	orphan.Name = "/cc-runtime-dev-1"
	orphan.NetworkSettings.Networks = map[string]struct {
		IPAddress string `json:"IPAddress"`
	}{"cc-runtime_private": {IPAddress: "172.30.0.10"}}
	got := AddressConflicts(project(t.TempDir()), []string{"dev-pennsylvania", "dev-california"}, []Container{orphan})
	if !slices.Equal(got, []string{"dev-pennsylvania 需要 172.30.0.10，但它仍被 cc-runtime-dev-1 占用"}) {
		t.Fatal(got)
	}
}

func machineIDSetup(t *testing.T, container string, copied string) (config.Config, string, Service) {
	base := t.TempDir()
	cfg := testutil.MakeConfig(t, base, "")
	path := filepath.Join(base, "machine-id")
	savedRequire, savedExisting, savedRun := require, existingContainer, runCommand
	t.Cleanup(func() { require, existingContainer, runCommand = savedRequire, savedExisting, savedRun })
	require = func(string) (string, error) { return "docker", nil }
	existingContainer = func(config.Config, string) (string, error) { return container, nil }
	runCommand = func(args []string, _ util.Opts) (util.Result, error) {
		if copied == "" {
			t.Fatalf("不应运行 %v", args)
		}
		return util.Result{}, os.WriteFile(args[len(args)-1], []byte(copied), 0o644)
	}
	return cfg, path, Service{Volumes: []Mount{{Type: "bind", Source: path, Target: "/etc/machine-id"}}}
}

func TestNewMachineGetsFreshIDAndExistingIDIsKept(t *testing.T) {
	cfg, path, service := machineIDSetup(t, "", "")
	if err := InitializeMachineID(cfg, "dev-X", service); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(path)
	if !regexp.MustCompile(`^[0-9a-f]{32}\n$`).Match(first) {
		t.Fatal(string(first))
	}
	InitializeMachineID(cfg, "dev-X", service)
	if again, _ := os.ReadFile(path); string(again) != string(first) {
		t.Fatal("machine-id 变了")
	}
}

func TestExistingContainerIDIsKept(t *testing.T) {
	cfg, path, service := machineIDSetup(t, "abc123", "0123456789abcdef0123456789abcdef\n")
	if err := InitializeMachineID(cfg, "dev-X", service); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "0123456789abcdef0123456789abcdef\n" {
		t.Fatal(string(got))
	}
}

func TestInvalidExistingFileIsRefused(t *testing.T) {
	cfg, path, service := machineIDSetup(t, "", "")
	os.WriteFile(path, nil, 0o644)
	if err := InitializeMachineID(cfg, "dev-X", service); err == nil || !strings.Contains(err.Error(), "不是有效的") {
		t.Fatal(err)
	}
}

// ---- Docker 集成测试（CC_RUNTIME_DOCKER_TEST=1 时运行） ----

func docker(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("docker %v：%v\n%s", args, err, out)
	}
}

func TestSSHKeysAreMigratedFromAStoppedContainerAndThenKept(t *testing.T) {
	if !dockerTests {
		t.Skip("Docker 集成测试需要 CC_RUNTIME_DOCKER_TEST=1")
	}
	name := fmt.Sprintf("cc-identity-test-%d", os.Getpid())
	defer exec.Command("docker", "rm", "-f", name).Run()
	docker(t, "run", "--name", name, "--network", "none", "--user", "0:0", "--entrypoint", "/bin/sh", testImage(),
		"-c", "rm -f /etc/ssh/ssh_host_*; ssh-keygen -A")
	base := t.TempDir()
	cfg := testutil.MakeConfig(t, base, "")
	directory := filepath.Join(base, "keys")
	service := Service{Volumes: []Mount{{Type: "bind", Source: directory, Target: SSHTarget}}}
	saved := existingContainer
	defer func() { existingContainer = saved }()
	existingContainer = func(config.Config, string) (string, error) { return name, nil }
	if err := InitializeSSHKeys(cfg, testImage(), "dev-X", service); err != nil {
		t.Fatal(err)
	}
	keys, _ := filepath.Glob(filepath.Join(directory, "ssh_host_*_key"))
	if len(keys) != 3 {
		t.Fatal(keys)
	}
	original := map[string]string{}
	for _, k := range keys {
		data, _ := os.ReadFile(k)
		original[k] = string(data)
	}
	existingContainer = func(config.Config, string) (string, error) { t.Fatal("应复用已保存的密钥"); return "", nil }
	if err := InitializeSSHKeys(cfg, testImage(), "dev-X", service); err != nil {
		t.Fatal(err)
	}
	for k, v := range original {
		if data, _ := os.ReadFile(k); string(data) != v {
			t.Fatal("密钥被替换了")
		}
	}
	os.WriteFile(keys[0], []byte("invalid key\n"), 0o600) // 损坏的身份必须报错，而不是换新
	if err := InitializeSSHKeys(cfg, testImage(), "dev-X", service); err == nil {
		t.Fatal("损坏的密钥应报错")
	}
}

func TestHomeIsSeededOnceWithoutOverwriting(t *testing.T) {
	if !dockerTests {
		t.Skip("Docker 集成测试需要 CC_RUNTIME_DOCKER_TEST=1")
	}
	image := fmt.Sprintf("cc-home-test-%d", os.Getpid())
	root := t.TempDir()
	home := filepath.Join(root, "home")
	os.Mkdir(home, 0o755)
	os.WriteFile(filepath.Join(home, ".zshrc"), []byte("keep-user-config"), 0o644)
	os.WriteFile(filepath.Join(root, "Dockerfile"), []byte(`FROM python:3.12-slim
RUN mkdir -p /home/dev/.local/bin /home/dev/.cache /home/dev/workspace && \
    echo image-config > /home/dev/.zshrc && echo image-tool > /home/dev/.local/bin/tool && \
    echo no-copy > /home/dev/.cache/image-cache && echo no-copy > /home/dev/workspace/image-source
`), 0o644)
	defer exec.Command("docker", "image", "rm", image).Run()
	docker(t, "build", "-q", "-t", image, root)
	read := func(rel string) string {
		data, _ := os.ReadFile(filepath.Join(home, rel))
		return strings.TrimSpace(string(data))
	}
	if err := InitializeHome(image, home, 1234, 1234); err != nil {
		t.Fatal(err)
	}
	if read(".zshrc") != "keep-user-config" || read(".local/bin/tool") != "image-tool" ||
		util.Lexists(filepath.Join(home, ".cache/image-cache")) || util.Lexists(filepath.Join(home, "workspace/image-source")) {
		t.Fatal("家目录初始化结果不对")
	}
	os.WriteFile(filepath.Join(home, ".local/bin/tool"), []byte("user-updated-tool"), 0o755)
	InitializeHome(image, home, 1234, 1234)
	if read(".local/bin/tool") != "user-updated-tool" {
		t.Fatal("第二次初始化覆盖了使用者的文件")
	}
}

func TestScreenshotsMount(t *testing.T) {
	base := t.TempDir()
	cfg := testutil.MakeConfig(t, base, "")
	shots := filepath.Join(base, "shots")
	inDownloads := filepath.Join(cfg.Downloads, "shots")
	for _, dir := range []string{shots, inDownloads} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	real, _ := filepath.EvalSymlinks(shots)
	cases := []struct {
		name, path, want string
		fails            bool
	}{
		{"未配置", "", "", false},
		{"目录不存在", filepath.Join(base, "missing"), "", false},
		{"独立目录", shots, real, false},
		{"在下载目录里", inDownloads, "", false},
		{"在工作区里", cfg.Workspace, "", false},
		{"包含工作区", base, "", true},
		{"在 RUNTIME_DIR 里", cfg.GitStore(), "", true},
	}
	for _, c := range cases {
		cfg.Screenshots = c.path
		got, err := ScreenshotsMount(cfg)
		if got != c.want || (err != nil) != c.fails {
			t.Errorf("%s：得到 %q, %v", c.name, got, err)
		}
	}
}

func TestComposeAddsScreenshotsOverlayForDevMachines(t *testing.T) {
	base := t.TempDir()
	cfg := testutil.MakeConfig(t, base, "")
	// 配置的路径经过一个链接：容器里用配置的路径，源用真实路径。
	if err := os.MkdirAll(filepath.Join(base, "shots$1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "shots$1"), filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	cfg.Screenshots = filepath.Join(base, "link")
	model := `{"name":"p","services":{
		"dev-a":{"volumes":[{"type":"bind","source":"/k","target":"` + SSHTarget + `"}]},
		"proxy-a":{"volumes":[]}}}`
	savedRequire, savedRun := require, runCommand
	t.Cleanup(func() { require, runCommand = savedRequire, savedRun })
	require = func(string) (string, error) { return "docker", nil }
	var last []string
	runCommand = func(args []string, _ util.Opts) (util.Result, error) {
		last = args
		return util.Result{Stdout: model}, nil
	}
	if _, err := compose(cfg, true, false, "ps"); err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(cfg.StateDir(), "compose.overlay.json")
	want := []string{"docker", "compose", "--project-directory", cfg.Root,
		"-f", filepath.Join(cfg.Root, "compose.yaml"), "-f", overlay, "ps"}
	if !slices.Equal(last, want) {
		t.Fatalf("命令是 %v", last)
	}
	data, err := os.ReadFile(overlay)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	real, _ := filepath.EvalSymlinks(cfg.Screenshots)
	escape := func(p string) string { return `"` + strings.ReplaceAll(p, "$", "$$") + `"` }
	if !strings.Contains(text, `"dev-a"`) || strings.Contains(text, "proxy-a") ||
		!strings.Contains(text, `"source": `+escape(real)) || !strings.Contains(text, `"target": `+escape(cfg.Screenshots)) ||
		!strings.Contains(text, `"read_only": true`) {
		t.Fatal(text)
	}

	// 目录不存在时只用 compose.yaml。
	cfg.Screenshots = filepath.Join(base, "missing")
	if _, err := compose(cfg, true, false, "ps"); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(last, overlay) {
		t.Fatalf("命令是 %v", last)
	}
}

func TestOverlayForRemoteShellMachines(t *testing.T) {
	base := t.TempDir()
	cfg := testutil.MakeConfig(t, base, "")
	gateway := "172.30.0.4"
	project := Project{Name: "p", Services: map[string]Service{
		"dev-a": {Volumes: []Mount{{Type: "bind", Source: "/k", Target: SSHTarget},
			{Type: "bind", Source: cfg.LinuxWorkspace(), Target: cfg.Workspace}}},
		"dev-b":              {Volumes: []Mount{{Type: "bind", Source: "/k2", Target: SSHTarget}}},
		"dev-a-network-init": {Environment: map[string]*string{"GATEWAY_IP": &gateway}},
		"proxy-a":            {},
	}}
	if got := GatewayIP(project, "dev-a"); got != gateway {
		t.Fatalf("网关 %s", got)
	}
	if got := GatewayIP(project, "dev-b"); got != "172.30.0.2" {
		t.Fatalf("默认网关 %s", got)
	}
	overlay, err := buildOverlay(cfg, project, "", []string{"dev-a"})
	if err != nil {
		t.Fatal(err)
	}
	services := overlay["services"].(map[string]any)
	if _, ok := services["dev-b"]; ok || len(services) != 1 {
		t.Fatalf("只应改 dev-a：%v", services)
	}
	text := fmt.Sprint(services["dev-a"])
	for _, want := range []string{"target:" + cfg.Workspace, "source:" + util.Resolve(cfg.Workspace),
		"target:/var/lib/cc-runtime/remote-shell", "target:/opt/cc-runtime", "read_only:true"} {
		if !strings.Contains(text, want) {
			t.Fatalf("缺少 %s：%s", want, text)
		}
	}
	if strings.Contains(text, "linux-workspace") {
		t.Fatalf("工作区不应再挂 Linux 副本：%s", text)
	}
	if _, err := buildOverlay(cfg, project, "", []string{"proxy-a"}); err == nil {
		t.Fatal("非开发机应该报错")
	}
}
