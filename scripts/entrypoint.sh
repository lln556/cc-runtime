#!/bin/bash
set -e

USER_PASSWORD=${PASSWORD:-"DefaultPasswordPleaseChange"}
SSH_PORT=${SSH_PORT:-22}
VNC_PORT=${VNC_PORT:-6080}
VNC_DISPLAY_PORT=${VNC_DISPLAY_PORT:-5901}

echo "dev:$USER_PASSWORD" | sudo chpasswd
echo "✅ User 'dev' password set."

# 工作区和下载目录挂在和 Mac 相同的路径（compose 传入 WORKSPACE_DIR、
# DOWNLOADS_DIR），路径两侧通用。家目录里的同名入口换成指向它们的链接。
link_home_dir() {
    local target=$1 home_path=$2
    if [ ! -d "$target" ]; then
        echo "📂 Creating directory $target..."
        sudo mkdir -p "$target"
        sudo chown dev:dev "$target"
    fi
    [ "$target" = "$home_path" ] && return 0
    # 以前的挂载点在持久家目录里留下一个空目录；换成链接。不空就不动。
    if [ -d "$home_path" ] && [ ! -L "$home_path" ]; then
        rmdir "$home_path" 2>/dev/null || echo "⚠️  $home_path is not empty; leaving it in place."
    fi
    if [ ! -e "$home_path" ] && [ ! -L "$home_path" ]; then
        echo "🔗 Creating symlink $home_path -> $target..."
        ln -s "$target" "$home_path"
    fi
}
WORKSPACE_DIR=${WORKSPACE_DIR:-/home/dev/workspace}
link_home_dir "$WORKSPACE_DIR" /home/dev/workspace
link_home_dir "${DOWNLOADS_DIR:-/home/dev/Downloads}" /home/dev/Downloads
# 当前目录（镜像的 WORKDIR）可能正是刚换掉的目录；重新进入，后面的进程才取得到它。
cd /home/dev/workspace 2>/dev/null || cd /home/dev

if [ ! -e "/workspace" ] && [ ! -L "/workspace" ]; then
    echo "🔗 Creating symlink /workspace -> $WORKSPACE_DIR..."
    sudo ln -s "$WORKSPACE_DIR" /workspace
fi
sudo chown -h dev:dev /workspace

# 写在家目录：工作区是 Mac 的目录，不往里放开发机的文件。
README_FILE="/home/dev/环境说明.md"
if [ ! -f "$README_FILE" ]; then
    echo "📝 Generating welcome file 环境说明.md..."
    cat <<'EOF' | sudo tee "$README_FILE" > /dev/null
# 欢迎使用团队 AI 云端工作站

您正在使用基于 Docker 构建的 **AI 编码 3.0** 环境。

## 快速提示
1. **AI 助手工具**  
   在 SSH 终端中使用：
   ```bash
   # Claude Code
   claude    # 别名默认添加 --dangerously-skip-permissions
   ```
   此别名下，AI 可以无需人工确认直接执行命令，请谨慎使用。

## 预装工具 (常见版本)
| 工具 | 版本 | 说明 |
|------|------|------|
| Ubuntu | 26.04 | 基础镜像 |
| **Zsh** + zim | 5.x | 默认 Shell |
| Bash | 5.x | 仍可使用 |
| OpenSSH Server | 最新 | 方便远程 SSH 登录 |
| **Node.js** | 23.x | 由 NodeSource 仓库安装 |
| pnpm | 最新 | 全局包管理器 |
| **Bun** | 最新稳定版 | JavaScript / TypeScript 运行时和包管理器，含 `bunx` |
| **Playwright** | 最新 | 以及 Chromium 浏览器 |
| xfce4 / TigerVNC / noVNC | 最新 | 远程桌面环境 |
| **Claude Code** | 最新 | 官方原生二进制，`claude` 别名默认跳过权限确认 |
| **uv** | 最新 | Python 包管理、虚拟环境和 Python 版本管理 |
| **Go** | 最新稳定版 | 官方二进制，`/usr/local/go` |
| **Rust** | stable | rustup，工具链在 `~/.cargo` |
| Git / Vim / curl / build-essential | - | 常用开发工具 |
| ripgrep | 最新 | 命令 `rg`，按内容搜索代码 |
| fonts-noto-cjk | - | 中文字体 |
| fzf | 最新 | 模糊查找，`Ctrl-R` / `Ctrl-T` |
| zoxide | 最新 | 目录跳转，命令 `z`，别名 `j` |
| nload | 最新 | 实时网卡流量 |
| btop | 系统仓库版本 | CPU、内存、磁盘和网络资源监控 |
| htop | 系统仓库版本 | 交互式进程查看和管理 |
| Herdr | 最新 | 终端复用器 |
| tmux | 系统仓库版本 | 终端复用、分屏和持久会话 |

> 注：版本号可能随镜像重新构建而更新，可在终端通过 `node -v`、`bun --version`、`uv --version`、`go version`、`rustc --version`、`claude --version` 等命令查看。

## 目录结构
- `/home/dev`：您的主目录。
- 工作区：直接挂载 Mac 的工作区，路径和 Mac 相同。Claude Code 的 Bash 命令经 remote-shell 在 Mac 上执行。
- `/home/dev/workspace`、`/workspace`：指向工作区的符号链接。
- 下载目录：挂在和 Mac 相同的路径上，`/home/dev/Downloads` 是指向它的符号链接。

祝你编码愉快！
EOF
    sudo chown dev:dev "$README_FILE"
fi

# Machine identity normally comes from a per-machine mount. If the image left
# /etc/machine-id empty and nothing is mounted, generate one for this container.
# A read-only mount is never rewritten: --ensure keeps any valid existing ID.
if ! sudo test -s /etc/machine-id; then
    sudo dbus-uuidgen --ensure=/etc/machine-id
fi
if ! sudo test -e /var/lib/dbus/machine-id; then
    sudo mkdir -p /var/lib/dbus
    sudo ln -s /etc/machine-id /var/lib/dbus/machine-id
fi

# Keep deployment identity outside the replaceable image and user home.
sudo bash <<'SSH_KEYS'
set -eu
key_dir=/var/lib/cc-runtime/ssh
install -d -m 700 -o root -g root "$key_dir"
umask 077
for type in rsa ecdsa ed25519; do
    key="$key_dir/ssh_host_${type}_key"
    if [ ! -e "$key" ]; then
        ssh-keygen -q -t "$type" -N '' -f "$key"
    fi
    chown root:root "$key"
    chmod 600 "$key"
    # Fail on corrupt keys rather than silently changing the host identity.
    ssh-keygen -y -f "$key" > "$key.pub"
    chmod 644 "$key.pub"
done
mkdir -p /etc/ssh/sshd_config.d
cat > /etc/ssh/sshd_config.d/00-cc-hostkeys.conf <<'CONFIG'
HostKey /var/lib/cc-runtime/ssh/ssh_host_rsa_key
HostKey /var/lib/cc-runtime/ssh/ssh_host_ecdsa_key
HostKey /var/lib/cc-runtime/ssh/ssh_host_ed25519_key
CONFIG
SSH_KEYS

sudo sed -i "s/^#*Port .*/Port $SSH_PORT/" /etc/ssh/sshd_config
sudo mkdir -p /run/sshd
sudo /usr/sbin/sshd -t
sudo /etc/init.d/ssh start
echo "✅ SSH server started on port $SSH_PORT."

VNC_CONFIG_DIR=/home/dev/.config/tigervnc
mkdir -p "$VNC_CONFIG_DIR"
echo "$USER_PASSWORD" | vncpasswd -f > "$VNC_CONFIG_DIR/passwd"
chmod 600 "$VNC_CONFIG_DIR/passwd"
VNC_DISPLAY_NUM=${PORT_BASE:-1}
vncserver :$VNC_DISPLAY_NUM -geometry 1280x800 -rfbport $VNC_DISPLAY_PORT -localhost no \
    -rfbauth "$VNC_CONFIG_DIR/passwd" -xstartup /usr/bin/startxfce4
echo "✅ VNC server started on display :$VNC_DISPLAY_NUM, port $VNC_DISPLAY_PORT."

websockify --web=/usr/share/novnc/ $VNC_PORT localhost:$VNC_DISPLAY_PORT &
echo "✅ noVNC (Web VNC client) started on port $VNC_PORT."

CLAUDE_DIR="/home/dev/.claude"
CLAUDE_FILE="/home/dev/.claude.json"
if [ -e "$CLAUDE_DIR" ]; then
    sudo chown -R dev:dev "$CLAUDE_DIR"
fi
if [ -e "$CLAUDE_FILE" ]; then
    sudo chown dev:dev "$CLAUDE_FILE"
fi

echo "🚀 Cloud Dev Environment is ready. Welcome to AI Coding 3.0."
tail -f /dev/null 
