#!/bin/bash
# 为当前用户安装 zim，并写入 .zshenv / .zimrc / .zshrc。
# 构建镜像时以 dev 用户运行，模块会预先装进 ~/.zim。
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export ZIM_HOME="${ZDOTDIR:-$HOME}/.zim"

cp "$SCRIPT_DIR/zshenv" "$HOME/.zshenv"
cp "$SCRIPT_DIR/zimrc" "$HOME/.zimrc"
cp "$SCRIPT_DIR/zshrc" "$HOME/.zshrc"

# Ghostty SSH sessions need their native cursor and redraw capabilities.
tic -x -o "$HOME/.terminfo" "$SCRIPT_DIR/xterm-ghostty.terminfo"
infocmp -x xterm-ghostty >/dev/null

mkdir -p "$ZIM_HOME"
# GitHub 经代理时 HTTP/2 容易被中途断开，下载和随后的 git clone 都走 HTTP/1.1。
git config --global http.version HTTP/1.1
curl --http1.1 --retry 5 --retry-all-errors --retry-delay 2 -fsSL \
    -o "$ZIM_HOME/zimfw.zsh" \
    https://github.com/zimfw/zimfw/releases/latest/download/zimfw.zsh

zsh -c "source ${ZIM_HOME}/zimfw.zsh install"
