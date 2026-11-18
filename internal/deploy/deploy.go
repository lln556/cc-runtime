// Package deploy 用 Docker Compose 启动部署，并保留每台机器的身份。
//
// Compose 创建开发机之前，先在宿主机上准备它的持久部分：家目录只从镜像初始化
// 一次；machine-id 和 SSH 主机密钥保留下来（从同一服务的现有容器迁移，新机器则
// 新建）；缓存卷和它的子路径存在。
package deploy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/zeke-chin/cc-runtime/internal/config"
	"github.com/zeke-chin/cc-runtime/internal/remoteshell"
	"github.com/zeke-chin/cc-runtime/internal/util"
)

const (
	SSHTarget  = "/var/lib/cc-runtime/ssh"
	HomeTarget = "/home/dev"
)

var creatingSubcommands = map[string]bool{"up": true, "create": true, "run": true}

type Mount struct {
	Type   string `json:"type"`
	Source string `json:"source"`
	Target string `json:"target"`
	Volume *struct {
		Subpath string `json:"subpath"`
	} `json:"volume,omitempty"`
}

type Network struct {
	IPv4Address string `json:"ipv4_address"`
}

type Service struct {
	Image       string              `json:"image"`
	Volumes     []Mount             `json:"volumes"`
	Networks    map[string]*Network `json:"networks"`
	Environment map[string]*string  `json:"environment"`
}

type Project struct {
	Name     string             `json:"name"`
	Services map[string]Service `json:"services"`
	Volumes  map[string]struct {
		Name string `json:"name"`
	} `json:"volumes"`
}

func (p Project) ServiceNames() []string {
	names := make([]string, 0, len(p.Services))
	for name := range p.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// 这几个函数可在测试中替换。
var (
	runCommand = util.Run
	require    = util.Require
	compose    = func(cfg config.Config, capture, allowFail bool, args ...string) (util.Result, error) {
		files, err := composeFiles(cfg)
		if err != nil {
			return util.Result{}, err
		}
		return composeWith(cfg, files, capture, allowFail, args...)
	}
)

func composeWith(cfg config.Config, files []string, capture, allowFail bool, args ...string) (util.Result, error) {
	docker, err := require("docker")
	if err != nil {
		return util.Result{}, err
	}
	command := []string{docker, "compose", "--project-directory", cfg.Root}
	for _, f := range files {
		command = append(command, "-f", f)
	}
	return runCommand(append(command, args...), util.Opts{Capture: capture, AllowFail: allowFail})
}

// composeFiles 是本部署的 Compose 文件：compose.yaml，以及需要时生成的叠加文件
// （截图目录、启用 remote-shell 的开发机）。
func composeFiles(cfg config.Config) ([]string, error) {
	base := filepath.Join(cfg.Root, "compose.yaml")
	dir, err := ScreenshotsMount(cfg)
	if err != nil {
		return nil, err
	}
	if dir == "" {
		return []string{base}, nil
	}
	result, err := composeWith(cfg, []string{base}, true, false, "config", "--format", "json")
	if err != nil {
		return nil, err
	}
	var project Project
	if err := json.Unmarshal([]byte(result.Stdout), &project); err != nil {
		return nil, fmt.Errorf("无法解析 docker compose config 的输出：%w", err)
	}
	data, err := json.MarshalIndent(buildOverlay(cfg, project, dir), "", "  ")
	if err != nil {
		return nil, err
	}
	path := filepath.Join(cfg.StateDir(), "compose.overlay.json")
	if err := os.MkdirAll(cfg.StateDir(), 0o755); err != nil {
		return nil, err
	}
	// Compose 会对值里的 $ 做变量替换，JSON 里也一样；路径里的 $ 要转义。
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(data), "$", "$$")), 0o644); err != nil {
		return nil, err
	}
	return []string{base, path}, nil
}

// buildOverlay 是截图目录的叠加配置：在每台开发机里以相同路径只读挂载。
func buildOverlay(cfg config.Config, project Project, screenshots string) map[string]any {
	// 源用真实路径；容器里用配置的路径，使用者粘贴的就是它。
	mount := screenshotsVolume(screenshots, filepath.Clean(cfg.Screenshots))
	services := map[string]any{}
	for _, name := range DevServices(project) {
		services[name] = map[string]any{"volumes": []any{mount}}
	}
	return map[string]any{"services": services}
}

// GatewayIP 是开发机的网关地址：取自它的 network-init 服务（与 scripts/dev-network-init.sh 的默认值一致）。
func GatewayIP(p Project, machine string) string {
	if helper, ok := p.Services[machine+"-network-init"]; ok {
		if value := helper.Environment["GATEWAY_IP"]; value != nil && *value != "" {
			return *value
		}
	}
	return "172.30.0.2"
}

// ScreenshotsMount 返回要挂进开发机的截图目录（真实路径）。没配置、目录不存在，
// 或它已经在工作区、下载目录里（开发机里本来就能看到）时返回空。
func ScreenshotsMount(cfg config.Config) (string, error) {
	if cfg.Screenshots == "" {
		return "", nil
	}
	dir, err := filepath.EvalSymlinks(cfg.Screenshots)
	if err != nil {
		return "", nil
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", nil
	}
	real := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return filepath.Clean(p)
	}
	workspace, downloads, runtime := real(cfg.Workspace), real(cfg.Downloads), real(cfg.Runtime)
	for _, covered := range []string{workspace, downloads} {
		if within(dir, covered) {
			return "", nil
		}
	}
	for _, inner := range []string{workspace, downloads, runtime} {
		if within(inner, dir) {
			return "", util.Errorf("截图目录 %s 包含了工作区、下载目录或 RUNTIME_DIR，不能挂载；请把 HOST_SCREENSHOTS_PATH 改成具体的截图目录", dir)
		}
	}
	if within(dir, runtime) {
		return "", util.Errorf("截图目录 %s 在 RUNTIME_DIR 里，不能挂载", dir)
	}
	return dir, nil
}

// within 判断 path 是否是 dir 本身或在 dir 之内。
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// screenshotsVolume 在相同路径上只读挂载截图目录。
func screenshotsVolume(source, target string) map[string]any {
	return map[string]any{"type": "bind", "source": source, "target": target, "read_only": true,
		"bind": map[string]any{"create_host_path": false}}
}

func Model(cfg config.Config) (Project, error) {
	result, err := compose(cfg, true, false, "config", "--format", "json")
	if err != nil {
		return Project{}, err
	}
	var project Project
	if err := json.Unmarshal([]byte(result.Stdout), &project); err != nil {
		return Project{}, fmt.Errorf("无法解析 docker compose config 的输出：%w", err)
	}
	return project, nil
}

// DevServices 是开发机服务（它们保存 SSH 主机密钥）。
func DevServices(p Project) []string {
	var names []string
	for _, name := range p.ServiceNames() {
		if MountFor(p.Services[name], SSHTarget) != nil {
			names = append(names, name)
		}
	}
	return names
}

// WithHelpers 加上每台机器的 network-init 辅助容器，它要在机器启动后运行。
func WithHelpers(p Project, services []string) []string {
	result := append([]string(nil), services...)
	seen := map[string]bool{}
	for _, s := range result {
		seen[s] = true
	}
	for _, name := range services {
		helper := name + "-network-init"
		if _, ok := p.Services[helper]; ok && !seen[helper] {
			result = append(result, helper)
			seen[helper] = true
		}
	}
	return result
}

type Container struct {
	Name   string `json:"Name"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
	Mounts []struct {
		Type        string `json:"Type"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
	} `json:"Mounts"`
}

func Inspect(ids []string) ([]Container, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	docker, err := require("docker")
	if err != nil {
		return nil, err
	}
	result, err := runCommand(append([]string{docker, "inspect"}, ids...), util.Opts{Capture: true})
	if err != nil {
		return nil, err
	}
	var containers []Container
	return containers, json.Unmarshal([]byte(result.Stdout), &containers)
}

func projectContainers(p Project) ([]Container, error) {
	docker, err := require("docker")
	if err != nil {
		return nil, err
	}
	result, err := runCommand([]string{docker, "ps", "-q", "--filter", "label=com.docker.compose.project=" + p.Name},
		util.Opts{Capture: true})
	if err != nil {
		return nil, err
	}
	return Inspect(strings.Fields(result.Stdout))
}

// orphanContainers 是本项目中服务已不在 compose.yaml 里的运行中容器。
func orphanContainers(p Project, containers []Container) []Container {
	var orphans []Container
	for _, c := range containers {
		if _, ok := p.Services[c.Config.Labels["com.docker.compose.service"]]; !ok {
			orphans = append(orphans, c)
		}
	}
	return orphans
}

func AddressConflicts(p Project, services []string, orphans []Container) []string {
	taken := map[string]string{}
	for _, c := range orphans {
		for _, network := range c.NetworkSettings.Networks {
			if network.IPAddress != "" {
				taken[network.IPAddress] = strings.TrimPrefix(c.Name, "/")
			}
		}
	}
	var conflicts []string
	for _, name := range services {
		networks := p.Services[name].Networks
		keys := make([]string, 0, len(networks))
		for key := range networks {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if networks[key] == nil {
				continue
			}
			if owner, ok := taken[networks[key].IPv4Address]; ok {
				conflicts = append(conflicts, fmt.Sprintf("%s 需要 %s，但它仍被 %s 占用", name, networks[key].IPv4Address, owner))
			}
		}
	}
	return conflicts
}

// ---- 每台机器的状态 ----------------------------------------------------------

func MountFor(s Service, target string) *Mount {
	for i := range s.Volumes {
		if s.Volumes[i].Target == target {
			return &s.Volumes[i]
		}
	}
	return nil
}

func imageIDs(image string) (int, int, error) {
	docker, err := require("docker")
	if err != nil {
		return 0, 0, err
	}
	if _, err := runCommand([]string{docker, "image", "inspect", "--format", "{{.Id}}", image}, util.Opts{Capture: true}); err != nil {
		return 0, 0, util.Errorf("本地没有镜像 %q；请加 --pull 运行", image)
	}
	result, err := runCommand([]string{docker, "run", "--rm", "--network", "none", "--entrypoint", "/bin/sh", image,
		"-c", "id -u dev; id -g dev"}, util.Opts{Capture: true})
	if err != nil {
		return 0, 0, err
	}
	ids := strings.Fields(result.Stdout)
	if len(ids) == 2 {
		uid, err1 := strconv.Atoi(ids[0])
		gid, err2 := strconv.Atoi(ids[1])
		if err1 == nil && err2 == nil {
			return uid, gid, nil
		}
	}
	return 0, 0, util.Errorf("无法从镜像中确定 dev 用户的 UID/GID")
}

const homeScript = `set -eu
if [ ! -f /persistent-home/.cc-runtime-home-initialized ]; then
    for path in /home/dev/.[!.]* /home/dev/..?* /home/dev/*; do
        [ -e "$path" ] || [ -L "$path" ] || continue
        # 镜像里可能带着每台机器的运行状态，从不复制。
        case "${path##*/}" in workspace|.cache|.claude|.claude.json) continue ;; esac
        cp -a -n "$path" /persistent-home/
    done
    # ~/workspace 由入口脚本建成指向工作区挂载点的链接。
    chown "$1:$2" /persistent-home
    touch /persistent-home/.cc-runtime-home-initialized
fi
`

// InitializeHome 从镜像初始化 bind mount 的家目录，只做一次，不覆盖使用者的文件。
func InitializeHome(image, home string, uid, gid int) error {
	if err := os.MkdirAll(home, 0o755); err != nil {
		return err
	}
	docker, err := require("docker")
	if err != nil {
		return err
	}
	_, err = runCommand([]string{docker, "run", "--rm", "--network", "none", "--user", "0:0",
		"--mount", "type=bind,source=" + home + ",target=/persistent-home",
		"--entrypoint", "/bin/sh", image, "-c", homeScript, "init", strconv.Itoa(uid), strconv.Itoa(gid)}, util.Opts{})
	return err
}

// existingContainer 可在测试中替换。
var existingContainer = func(cfg config.Config, service string) (string, error) {
	result, err := compose(cfg, true, false, "ps", "--all", "--quiet", service)
	if err != nil {
		return "", err
	}
	ids := strings.Fields(result.Stdout)
	if len(ids) > 1 {
		return "", util.Errorf("%s 容器最多应有一个", service)
	}
	if len(ids) == 0 {
		return "", nil
	}
	return ids[0], nil
}

var machineIDFile = regexp.MustCompile(`^[0-9a-f]{32}\n?$`)
var machineID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// InitializeMachineID 保留 machine-id：先用文件里的，其次用现有容器里的，最后新建。
func InitializeMachineID(cfg config.Config, name string, s Service) error {
	mount := MountFor(s, "/etc/machine-id")
	if mount == nil {
		return nil
	}
	if mount.Type != "bind" {
		return util.Errorf("%s 的 machine-id 必须用 bind mount", name)
	}
	path := mount.Source
	if data, err := os.ReadFile(path); err == nil {
		if !machineIDFile.Match(data) {
			return util.Errorf("%s 的 machine-id 不是有效的 ID：%s", name, path)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	container, err := existingContainer(cfg, name)
	if err != nil {
		return err
	}
	value := ""
	if container != "" {
		temporary, err := os.MkdirTemp("", "cc-machine-id-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(temporary)
		copied := filepath.Join(temporary, "machine-id")
		docker, err := require("docker")
		if err != nil {
			return err
		}
		if _, err := runCommand([]string{docker, "cp", "-L", container + ":/etc/machine-id", copied}, util.Opts{}); err != nil {
			return err
		}
		data, err := os.ReadFile(copied)
		if err != nil {
			return err
		}
		if v := strings.TrimSpace(string(data)); machineID.MatchString(v) {
			value = v
		}
	}
	action := "沿用已有的"
	if value == "" {
		action = "生成了新的"
		value = newMachineID()
	}
	if err := os.WriteFile(path, []byte(value+"\n"), 0o644); err != nil {
		return err
	}
	os.Chmod(path, 0o644)
	fmt.Printf("%s：%s machine-id。\n", name, action)
	return nil
}

func newMachineID() string {
	var b [16]byte
	f, err := os.Open("/dev/urandom")
	if err == nil {
		defer f.Close()
		f.Read(b[:])
	}
	// UUID v4 的位，与 uuid.uuid4().hex 相同的格式。
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x", b)
}

const sshCopyScript = `set -eu
umask 077
for type in rsa ecdsa ed25519; do
    ssh-keygen -y -f "/source/ssh_host_${type}_key" > /dev/null
    cp "/source/ssh_host_${type}_key" "/keys/ssh_host_${type}_key"
    chown root:root "/keys/ssh_host_${type}_key"
    chmod 600 "/keys/ssh_host_${type}_key"
done
`

// InitializeSSHKeys 保留 SSH 主机密钥：校验已保存的，或从容器里迁移。
func InitializeSSHKeys(cfg config.Config, image, name string, s Service) error {
	mount := MountFor(s, SSHTarget)
	if mount.Type != "bind" {
		return util.Errorf("SSH 主机密钥必须用 bind mount")
	}
	directory := util.Resolve(mount.Source)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	docker, err := require("docker")
	if err != nil {
		return err
	}
	helper := []string{docker, "run", "--rm", "--network", "none", "--user", "0:0",
		"--mount", "type=bind,source=" + directory + ",target=/keys"}
	with := func(extra ...string) []string { return append(append([]string(nil), helper...), extra...) }
	existing, err := runCommand(with("--entrypoint", "/bin/sh", image, "-c",
		`find /keys -name "ssh_host_*_key" -print`), util.Opts{Capture: true})
	if err != nil {
		return err
	}
	if strings.TrimSpace(existing.Stdout) != "" {
		_, err := runCommand(with("--entrypoint", "/bin/sh", image, "-c",
			`set -eu; for type in rsa ecdsa ed25519; do `+
				`ssh-keygen -y -f "/keys/ssh_host_${type}_key" > /dev/null; done`), util.Opts{})
		return err // 从不覆盖已经确立的身份
	}
	container, err := existingContainer(cfg, name)
	if err != nil || container == "" {
		return err // 新机器：入口脚本会在这个目录里生成密钥
	}
	containers, err := Inspect([]string{container})
	if err != nil {
		return err
	}
	old := "/etc/ssh"
	for _, c := range containers {
		for _, m := range c.Mounts {
			if m.Destination == SSHTarget {
				old = SSHTarget
			}
		}
	}
	temporary, err := os.MkdirTemp("", "cc-ssh-migration-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	source := filepath.Join(temporary, "ssh")
	if _, err := runCommand([]string{docker, "cp", container + ":" + old, source}, util.Opts{}); err != nil {
		return err
	}
	for _, kind := range []string{"rsa", "ecdsa", "ed25519"} {
		if info, err := os.Stat(filepath.Join(source, "ssh_host_"+kind+"_key")); err != nil || !info.Mode().IsRegular() {
			return util.Errorf("%s 的主机密钥不完整；拒绝替换它的身份", name)
		}
	}
	if _, err := runCommand(with("--mount", "type=bind,source="+source+",target=/source,readonly",
		"--entrypoint", "/bin/sh", image, "-c", sshCopyScript), util.Opts{}); err != nil {
		return err
	}
	fmt.Printf("%s：沿用已有的 SSH 主机密钥。\n", name)
	return nil
}

// initializeCache 创建开发机用到的命名卷和它们的子路径。
func initializeCache(p Project, image string, s Service, uid, gid int) error {
	docker, err := require("docker")
	if err != nil {
		return err
	}
	for _, mount := range s.Volumes {
		if mount.Type != "volume" {
			continue
		}
		name := p.Volumes[mount.Source].Name
		if name == "" {
			name = p.Name + "_" + mount.Source
		}
		if _, err := runCommand([]string{docker, "volume", "create", name}, util.Opts{Capture: true}); err != nil {
			return err
		}
		if mount.Volume != nil && mount.Volume.Subpath != "" {
			if _, err := runCommand([]string{docker, "run", "--rm", "--network", "none", "--user", "0:0",
				"--mount", "type=volume,source=" + name + ",target=/volume",
				"--entrypoint", "/bin/sh", image, "-c",
				`mkdir -p "/volume/$3" && chown "$1:$2" "/volume/$3"`, "init",
				strconv.Itoa(uid), strconv.Itoa(gid), mount.Volume.Subpath}, util.Opts{}); err != nil {
				return err
			}
		}
	}
	return nil
}

// Prepare 准备将要创建的开发机；services 为 nil 表示全部。
func Prepare(cfg config.Config, p Project, services []string) error {
	wanted := map[string]bool{}
	for _, s := range services {
		wanted[s] = true
	}
	var machines []string
	for _, name := range DevServices(p) {
		if services == nil || wanted[name] {
			machines = append(machines, name)
		}
	}
	shared := []string{filepath.Clean(cfg.Workspace), filepath.Clean(cfg.Downloads)}
	if dir, err := ScreenshotsMount(cfg); err == nil && dir != "" {
		shared = append(shared, filepath.Clean(cfg.Screenshots))
	}
	for _, name := range machines {
		s := p.Services[name]
		uid, gid, err := imageIDs(s.Image)
		if err != nil {
			return err
		}
		if home := MountFor(s, HomeTarget); home != nil {
			if home.Type != "bind" {
				return util.Errorf("%s 的家目录必须是 bind mount", name)
			}
			if err := InitializeHome(s.Image, home.Source, uid, gid); err != nil {
				return err
			}
		}
		if err := InitializeMachineID(cfg, name, s); err != nil {
			return err
		}
		if err := InitializeSSHKeys(cfg, s.Image, name, s); err != nil {
			return err
		}
		if err := initializeCache(p, s.Image, s, uid, gid); err != nil {
			return err
		}
		if _, err := remoteshell.PrepareMachine(cfg, remoteshell.Machine{Name: name, GatewayIP: GatewayIP(p, name),
			Container: p.Name + "-" + name + "-1", SharedDirs: shared}); err != nil {
			return err
		}
	}
	return nil
}

type UpOptions struct {
	Pull, ForceRecreate, AllowOrphans bool
}

func Up(cfg config.Config, services []string, opts UpOptions) error {
	project, err := Model(cfg)
	if err != nil {
		return err
	}
	var unknown []string
	for _, s := range services {
		if _, ok := project.Services[s]; !ok {
			unknown = append(unknown, s)
		}
	}
	if len(unknown) > 0 {
		return util.Errorf("未知服务：%s", strings.Join(unknown, ", "))
	}
	targets := project.ServiceNames()
	if len(services) > 0 {
		targets = WithHelpers(project, services)
	}
	containers, err := projectContainers(project)
	if err != nil {
		return err
	}
	orphans := orphanContainers(project, containers)
	if len(orphans) > 0 && !opts.AllowOrphans {
		var names []string
		for _, c := range orphans {
			names = append(names, strings.TrimPrefix(c.Name, "/"))
		}
		conflicts := AddressConflicts(project, targets, orphans)
		if len(conflicts) > 0 || len(services) == 0 {
			detail := "完整启动会与它们冲突"
			if len(conflicts) > 0 {
				detail = strings.Join(conflicts, "；")
			}
			return util.Errorf("有运行中的容器不在 compose.yaml 里（%s）：%s。请先停止它们，或只启动指定服务。",
				strings.Join(names, ", "), detail)
		}
	}
	if opts.Pull {
		pull := []string{"pull"}
		for _, s := range targets {
			if project.Services[s].Image != "" {
				pull = append(pull, s)
			}
		}
		if _, err := compose(cfg, false, false, pull...); err != nil {
			return err
		}
	}
	if err := Prepare(cfg, project, targets); err != nil {
		return err
	}
	args := []string{"up", "-d"}
	if len(services) > 0 {
		args = append(args, "--no-deps")
	}
	if opts.ForceRecreate {
		args = append(args, "--force-recreate")
	}
	_, err = compose(cfg, false, false, append(args, targets...)...)
	return err
}

// ComposePassthrough 运行任意 Compose 命令；会创建容器的命令先准备开发机。
func ComposePassthrough(cfg config.Config, args []string) (int, error) {
	creates := false
	for _, a := range args {
		if creatingSubcommands[a] {
			creates = true
		}
	}
	if creates {
		project, err := Model(cfg)
		if err != nil {
			return 1, err
		}
		var named []string
		for _, a := range args {
			if _, ok := project.Services[a]; ok {
				named = append(named, a)
			}
		}
		if err := Prepare(cfg, project, named); err != nil {
			return 1, err
		}
	}
	result, err := compose(cfg, false, true, args...)
	return result.Code, err
}

// Exec 在服务的容器里以 dev 用户运行命令，收集输出。
func Exec(cfg config.Config, service string, args ...string) (util.Result, error) {
	return compose(cfg, true, true, append([]string{"exec", "-T", "-u", "dev", service}, args...)...)
}
