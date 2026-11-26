#!/bin/bash
set -e

USER_PASSWORD=${PASSWORD:-"DefaultPasswordPleaseChange"}
SSH_PORT=${SSH_PORT:-22}

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
if [ -n "${PERSONAL_DIR:-}" ]; then
    link_home_dir "$PERSONAL_DIR" /home/dev/personal
fi
link_home_dir "${DOWNLOADS_DIR:-/home/dev/Downloads}" /home/dev/Downloads
# 当前目录（镜像的 WORKDIR）可能正是刚换掉的目录；重新进入，后面的进程才取得到它。
cd /home/dev/workspace 2>/dev/null || cd /home/dev

if [ ! -e "/workspace" ] && [ ! -L "/workspace" ]; then
    echo "🔗 Creating symlink /workspace -> $WORKSPACE_DIR..."
    sudo ln -s "$WORKSPACE_DIR" /workspace
fi
sudo chown -h dev:dev /workspace

# Claude Code 的临时目录因机器而异（Mac 上的路径），写成系统级设置的 drop-in；
# 其余系统级设置在 /etc/claude-code/managed-settings.json。
if [ -n "${CC_REMOTE_SHELL_TMPDIR:-}" ]; then
    sudo mkdir -p /etc/claude-code/managed-settings.d
    printf '{"env":{"CLAUDE_CODE_TMPDIR":"%s"}}\n' "$CC_REMOTE_SHELL_TMPDIR" \
        | sudo tee /etc/claude-code/managed-settings.d/remote-shell.json > /dev/null
    echo "✅ Claude Code temp dir: $CC_REMOTE_SHELL_TMPDIR"
else
    echo "⚠️  CC_REMOTE_SHELL_TMPDIR is not set; Claude Code scratchpad will not be shared with the Mac."
fi

# 写在家目录：工作区是 Mac 的目录，不往里放开发机的文件。每次启动重写，和镜像保持一致。
README_FILE="/home/dev/环境说明.md"
cat > "$README_FILE" <<'EOF'
# 开发机环境说明

这台开发机只运行 Claude Code。Claude Code 的 Bash 命令经 remote-shell 在宿主 Mac 上执行，
编译、测试、运行项目用的语言工具链都在 Mac 上，开发机里不装。

## 使用
```bash
cd <工作区里的项目>   # Claude Code 要在工作区或下载目录里启动
claude                # 别名默认添加 --dangerously-skip-permissions
```
此别名下，AI 可以无需人工确认直接执行命令，请谨慎使用。

## 预装工具
| 工具 | 说明 |
|------|------|
| Ubuntu 26.04 | 基础镜像 |
| Claude Code | 官方原生二进制，在 `~/.local/bin` |
| Zsh + zim | 默认 Shell；Bash 仍可使用 |
| Herdr | 终端复用器 |
| OpenSSH Server | SSH 登录 |
| Git、ripgrep | Claude Code 在容器里调用 |
| jq、Python 3 | Claude Code 的状态栏脚本使用 |
| Vim、curl、less | 常用工具 |

## 目录
- `/home/dev`：主目录，只在开发机里，Mac 上看不到。
- 工作区：直接挂载 Mac 的工作区，路径和 Mac 相同；`/home/dev/workspace`、`/workspace` 指向它。
- 下载目录：挂在和 Mac 相同的路径上，`/home/dev/Downloads` 指向它。
EOF

# Machine identity normally comes from a per-machine mount. If the image left
# /etc/machine-id empty and nothing is mounted, generate one for this container.
# A mounted ID is never rewritten: only an empty file gets a new one.
if ! sudo test -s /etc/machine-id; then
    tr -d '-' < /proc/sys/kernel/random/uuid | sudo tee /etc/machine-id > /dev/null
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

CLAUDE_DIR="/home/dev/.claude"
CLAUDE_FILE="/home/dev/.claude.json"
if [ -e "$CLAUDE_DIR" ]; then
    sudo chown -R dev:dev "$CLAUDE_DIR"
fi
if [ -e "$CLAUDE_FILE" ]; then
    sudo chown dev:dev "$CLAUDE_FILE"
fi

# 优先使用镜像内的 /opt/cc-runtime/antigravity；旧镜像未包含时回退到持久家目录里的副本。
AGY_WRAP_DIR="/opt/cc-runtime/antigravity"
if [ ! -x "$AGY_WRAP_DIR/agy" ] && [ -x /home/dev/.local/lib/agy-remote-shell/agy ]; then
    AGY_WRAP_DIR="/home/dev/.local/lib/agy-remote-shell"
fi
if [ -x "$AGY_WRAP_DIR/setup" ] && { [ -x /home/dev/.local/bin/agy ] || [ -d /home/dev/.gemini ]; }; then
    "$AGY_WRAP_DIR/setup" /home/dev
fi
if [ -x "$AGY_WRAP_DIR/agy" ] && [ -x /home/dev/.local/bin/agy ]; then
    sudo sed -i '/^PATH=/d' /etc/environment
    printf 'PATH="%s:/home/dev/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"\n' "$AGY_WRAP_DIR" \
        | sudo tee -a /etc/environment > /dev/null
fi

if [ -x /home/dev/.local/bin/cc-switch ]; then
    /home/dev/.local/bin/cc-switch daemon start --detach >/tmp/cc-switch-daemon.log 2>&1 || true
fi

if [ -n "${T3_WORKTREES_DIR:-}" ] && [ -d /home/dev/.t3 ]; then
    mkdir -p /home/dev/.t3/userdata
    python3 - "$T3_WORKTREES_DIR" <<'PY'
import json, os, sys
target = sys.argv[1]
path = "/home/dev/.t3/userdata/settings.json"
data = {}
if os.path.exists(path):
    try:
        with open(path, "r", encoding="utf-8") as f:
            loaded = json.load(f)
        if isinstance(loaded, dict):
            data = loaded
    except Exception:
        pass
if data.get("worktreesDirectory") != target:
    data["worktreesDirectory"] = target
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(data, f, indent=2, ensure_ascii=False)
        f.write("\n")
    os.replace(tmp, path)
PY
fi

if [ -f /home/dev/.t3/t3-lan-forwarder.py ]; then
    nohup python3 /home/dev/.t3/t3-lan-forwarder.py >/tmp/t3-lan-forwarder.log 2>&1 &
fi

if [ -x /home/dev/.t3/runtime/versions/0.1.11/t3 ]; then
    (
        cd /home/dev
        export SHELL=/bin/zsh
        export PATH="$AGY_WRAP_DIR:/home/dev/.local/bin:/home/dev/.local/share/swiftly/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
        nohup /home/dev/.t3/runtime/versions/0.1.11/t3 serve --host 127.0.0.1 --port 3773 --base-dir /home/dev/.t3 >> /home/dev/.t3/ssh-launch/c365cb87c3b38122/server.log 2>&1 </dev/null &
        echo $! > /home/dev/.t3/ssh-launch/c365cb87c3b38122/pid
    )
fi

echo "🚀 Cloud Dev Environment is ready. Welcome to AI Coding 3.0."
tail -f /dev/null 
