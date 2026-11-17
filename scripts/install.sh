#!/bin/sh
# 编译 cc-runtime 并安装到 ~/.local/bin（可用 PREFIX 改目录）。
# 二进制里记录本仓库的路径和提交号；launchd 任务直接调用它，不依赖 Go 或仓库里的任何环境。
# 更新代码后重新运行本脚本即可，已安装的定时任务不用重装。
# 同时交叉编译开发机用的 Linux 版（remote-shell 客户端）到 RUNTIME_DIR/bin，
# remote-shell 服务在运行时重启它（有命令正在运行时只提示，不中止它们）。
# 设置 CC_RUNTIME_CODESIGN_IDENTITY 时用该证书签名，系统给它的授权在重新编译后仍然有效。
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
prefix=${PREFIX:-$HOME/.local/bin}
command -v go >/dev/null 2>&1 || { echo "需要 Go（https://go.dev/dl/）" >&2; exit 1; }

version=$(git -C "$root" describe --always --dirty 2>/dev/null || echo unknown)
pkg=github.com/zeke-chin/cc-runtime/internal/config
mkdir -p "$prefix"
temporary="$prefix/.cc-runtime.$$"
trap 'rm -f "$temporary"' EXIT

cd "$root"
go build -trimpath -ldflags "-s -w -X $pkg.DefaultRoot=$root -X $pkg.Version=$version" \
    -o "$temporary" ./cmd/cc-runtime
if [ -n "${CC_RUNTIME_CODESIGN_IDENTITY:-}" ]; then
    codesign --force --sign "$CC_RUNTIME_CODESIGN_IDENTITY" --identifier io.cc-runtime "$temporary"
fi
# 先写临时文件再改名：正在运行的定时任务不受影响。
mv -f "$temporary" "$prefix/cc-runtime"
echo "已安装 ${prefix}/cc-runtime（${version}，仓库 ${root}）"

# 开发机用的 Linux 版：和 Mac 同一架构（开发机运行在本机的 Docker 虚拟机里）。
runtime_dir=$(sed -n 's/^RUNTIME_DIR=//p' "$root/.env" 2>/dev/null | tail -n 1 | sed -e 's/^["'\'']//' -e 's/["'\'']$//')
runtime_dir=${RUNTIME_DIR:-${runtime_dir:-$HOME/.cc-runtime}}
case $runtime_dir in "~"/*) runtime_dir=$HOME/${runtime_dir#"~/"} ;; esac
case $(uname -m) in
    arm64 | aarch64) arch=arm64 ;;
    *) arch=amd64 ;;
esac
bin_dir=$runtime_dir/bin
mkdir -p "$bin_dir/remote-shell"
GOOS=linux GOARCH=$arch CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X $pkg.Version=$version" -o "$bin_dir/.cc-runtime-linux.$$" ./cmd/cc-runtime
mv -f "$bin_dir/.cc-runtime-linux.$$" "$bin_dir/cc-runtime-linux"
# CLAUDE_CODE_SHELL 指向这个链接：Claude Code 只接受路径里含 bash 或 zsh 的 shell。
ln -sfn ../cc-runtime-linux "$bin_dir/remote-shell/zsh"
echo "已安装 ${bin_dir}/cc-runtime-linux（linux/${arch}，开发机的 remote-shell 客户端）"

if launchctl print "gui/$(id -u)/io.cc-runtime.remote-shell" >/dev/null 2>&1; then
    "$prefix/cc-runtime" remote-shell restart || echo "提示：remote-shell 服务仍是旧版本，确认后运行 cc-runtime remote-shell restart --force" >&2
fi
case ":$PATH:" in
    *":$prefix:"*) ;;
    *) echo "提示：${prefix} 不在 PATH 中" >&2 ;;
esac
