#!/usr/bin/env bash
# 示例脚本：备份（将 target 打包为 tar.gz 到 destination）
set -euo pipefail

TARGET="${PARAM_TARGET:-${1:-}}"
DEST="${PARAM_DESTINATION:-${2:-./backups}}"

[ -n "$TARGET" ] || { echo "缺少 target 参数" >&2; exit 1; }
[ -e "$TARGET" ] || { echo "目标不存在: $TARGET" >&2; exit 1; }

TS=$(date +%Y%m%d%H%M%S)
OUT="${DEST%/}/backup_${TS}.tar.gz"

mkdir -p "$(dirname "$OUT")"
tar -czf "$OUT" -C "$(dirname "$TARGET")" "$(basename "$TARGET")"

echo "备份完成: $OUT"
ls -lh "$OUT"
