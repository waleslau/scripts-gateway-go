#!/usr/bin/env bash
# 示例脚本：生成报告（演示 GET 方法任务）
set -euo pipefail

DAYS="${PARAM_DAYS:-${1:-7}}"

echo "最近 ${DAYS} 天运行报告"
echo "任务总数: 42"
echo "成功率: 99.2%"
echo "平均耗时: 3.1s"
