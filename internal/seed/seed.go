// Package seed 用已有开发机的家目录初始化新机器的家目录，但不带它的身份。
//
// 复制使用者安装的工具、shell 和编辑器配置，以及使用者选择共享的工具凭据
// （Credentials）。从不复制机器身份：SSH 主机密钥、machine-id、遥测 ID、整个
// ~/.claude 目录和 ~/.claude.json、历史和会话状态。新机器有自己的 SSH 主机密钥、
// machine-id 和 Claude 登录。
//
// 在 Docker 宿主机上运行。macOS（APFS）上用 `cp -c` 写时复制克隆：瞬间完成、
// 不占额外空间、两侧各自独立变化。Linux 上用 `cp --reflink=auto`。从不使用硬链接。
package seed

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/zeke-chin/cc-runtime/internal/util"
)

// Copy 是相对家目录的路径；不存在的跳过。
var Copy = []string{
	// Shell 和编辑器配置（不含历史）。
	".bashrc", ".bash_logout", ".profile", ".zshrc", ".zshenv", ".zimrc", ".zim",
	".vimrc",
	// Git 身份属于使用者，不属于机器。
	".gitconfig", ".gitconfig-weimob",
	// SSH：谁可以登录、信任哪些主机。客户端密钥见 Credentials。
	".ssh/authorized_keys", ".ssh/config", ".ssh/known_hosts",
	// 使用者安装的工具。语言工具链和包缓存在 Mac 上，开发机里不装。
	".local/bin", ".local/lib",
	// 不含凭据的工具偏好。
	".config/gh/config.yml",
}

// Credentials 是使用者选择在开发机之间共享的账号凭据（2026-11-12）。从不覆盖已有文件。
var Credentials = []string{
	".git-credentials", ".config/gh/hosts.yml", ".config/context7",
	".config/fast-context", ".config/salescli", ".tavily",
	".ssh/id_ed25519", ".ssh/id_ed25519.pub", ".ssh/id_rsa", ".ssh/id_rsa.pub",
}

// NotCopied 从不复制；列出来让这个决定明确、可审阅。
var NotCopied = [][2]string{
	{".claude.json", "Claude 的 userID、machineID 和 oauthAccount"},
	{".claude/", "Claude Code 的全部配置、登录、skills、插件和历史"},
	{".aliyun", "遥测实例 ID 和日志（不含凭据）"},
	{".config/go/telemetry", "Go 遥测状态"},
	{".local/state, .config/herdr, .servercc", "会话状态"},
	{".local/share/zoxide, .zhistory, .viminfo", "使用历史"},
	{".local/share/pki", "本机密钥（会重新生成）"},
	{".zcompdump*, .zcompcache", "可重新生成的缓存"},
}

// claudeCurrentVersion 只复制 ~/.local/bin/claude 指向的那个 Claude Code 版本。
func claudeCurrentVersion(source string) []string {
	link := filepath.Join(source, ".local/bin/claude")
	target, err := os.Readlink(link)
	if err != nil {
		return nil
	}
	target = strings.Replace(target, "/home/dev/", "", 1)
	if util.Lexists(filepath.Join(source, target)) {
		if _, err := os.Stat(filepath.Join(source, target)); err == nil {
			return []string{filepath.ToSlash(filepath.Clean(target))}
		}
	}
	return nil
}

func clone(source, destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	var cmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		cmd = exec.Command("cp", "-c", "-pR", source, destination)
	} else {
		cmd = exec.Command("cp", "-a", "--reflink=auto", source, destination)
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// present 判断路径存在（跟随链接），或者是一个链接（包括失效的）。
func present(path string) bool {
	if _, err := os.Stat(path); err == nil {
		return true
	}
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink != 0
}

type Options struct {
	DryRun          bool
	CredentialsOnly bool
	Out             func(string)
}

func Seed(source, destination string, opts Options) error {
	out := opts.Out
	if _, err := os.Stat(source); err != nil {
		return util.Errorf("来源家目录不存在：%s", source)
	}
	src, dst := util.Resolve(source), util.Resolve(destination)
	if dst == src || strings.HasPrefix(dst, src+"/") {
		return util.Errorf("目标必须在来源家目录之外")
	}
	var paths []string
	if opts.CredentialsOnly {
		if !util.IsDir(dst) {
			return util.Errorf("目标家目录不存在：%s", dst)
		}
		paths = Credentials
	} else {
		if entries, err := os.ReadDir(dst); err == nil && len(entries) > 0 {
			return util.Errorf("目标不为空：%s；拒绝合并家目录", dst)
		}
		paths = append(append(append([]string(nil), Copy...), claudeCurrentVersion(src)...), Credentials...)
	}
	for _, rel := range paths {
		if !present(filepath.Join(src, rel)) {
			out("跳过  " + rel + "（不存在）")
			continue
		}
		if present(filepath.Join(dst, rel)) {
			out("保留  " + rel + "（已存在）")
			continue
		}
		out("复制  " + rel)
		if !opts.DryRun {
			if err := clone(filepath.Join(src, rel), filepath.Join(dst, rel)); err != nil {
				return err
			}
		}
	}
	out("\n不复制（机器身份、历史或状态）：")
	for _, item := range NotCopied {
		out("  " + item[0] + "：" + item[1])
	}
	if !opts.DryRun {
		os.Chmod(dst, 0o755)
		if util.Lexists(filepath.Join(dst, ".ssh")) {
			os.Chmod(filepath.Join(dst, ".ssh"), 0o700)
		}
	}
	return nil
}
