#!/bin/sh
# 预热 dev-pennsylvania 容器里的 claude：发一条"只回复当前纽约时间"的请求，
# 输出追加到 cc-runtime 日志目录的 warm-claude-pa.log，退出码写入同名 .status 文件。
set -u

container=cc-runtime-dev-pennsylvania-1
log_dir=$HOME/.local/state/cc-runtime/logs
log_file=$log_dir/warm-claude-pa.log
status_file=$log_file.status
prompt='只回复当前纽约时间'

mkdir -p "$log_dir"

start_epoch=$(date +%s)
utc_now=$(date -u -r "$start_epoch" +%Y-%m-%dT%H:%M:%SZ)
echo "===== $utc_now =====" >>"$log_file"

if ! /usr/local/bin/docker inspect -f '{{.State.Running}}' "$container" 2>/dev/null | grep -q true; then
    echo "容器 $container 未运行，跳过" >>"$log_file"
    echo "skipped: container not running" >"$status_file"
    exit 0
fi

tmp_out=$(mktemp /tmp/warm-claude-pa.XXXXXX)
tmp_err=$(mktemp /tmp/warm-claude-pa.XXXXXX)

/usr/local/bin/docker exec \
    -e BASH_FUNC_claude%%= \
    -u dev \
    "$container" \
    sh -c 'cd ~ && timeout 120 claude -p "$0" --output-format text' "$prompt" \
    >"$tmp_out" 2>"$tmp_err"
exit_code=$?

elapsed=$(($(date +%s) - start_epoch))

if [ "$exit_code" -eq 0 ] && [ -s "$tmp_out" ]; then
    body=$(cat "$tmp_out")
else
    body=$(cat "$tmp_out" "$tmp_err" 2>/dev/null | tail -20)
fi

printf 'exit=%d elapsed=%ss\n%s\n' "$exit_code" "$elapsed" "$body" >>"$log_file"
printf 'exit=%d\nat=%s\n' "$exit_code" "$utc_now" >"$status_file"

rm -f "$tmp_out" "$tmp_err"
exit 0
