#!/usr/bin/env bash
# 示例脚本：部署（模拟）
# 参数通过环境变量注入：PARAM_ENV / PARAM_VERSION
# 也支持位置参数 $1=env $2=version
set -euo pipefail

ENV="${PARAM_ENV:-${1:-}}"
VERSION="${PARAM_VERSION:-${2:-}}"

[ -n "$ENV" ] || { echo "缺少 env 参数" >&2; exit 1; }
[ -n "$VERSION" ] || { echo "缺少 version 参数" >&2; exit 1; }

echo "开始部署 [环境=$ENV, 版本=$VERSION]"
sleep 2
echo "部署完成: $ENV/$VERSION"
