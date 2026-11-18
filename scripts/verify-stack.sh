#!/usr/bin/env bash
# 静态校验本地开发栈契约（不启动、不停止任何东西）。
#
# 用法：
#   scripts/verify-stack.sh
#
# 校验内容：
#   - scripts/dev-stack.json 可解析且严格合法（14 项、名称/端口/PID 文件唯一、
#     健康探测类型合法、配置模板成对声明）
#   - harnessloop 的健康探测必须指向 127.0.0.1:50013
#   - 契约与 scripts/dev-lib.sh / dev-down.sh / dev-up.sh 不漂移
#
# 真实冷启动验证由 T-P2-26 在独占窗口执行；本脚本只做静态检查，
# 因此可以安全地跑在 50013 栈正在被其他任务使用的机器上。

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1

CONTRACT="$ROOT/scripts/dev-stack.json"
if [ ! -f "$CONTRACT" ]; then
  echo "[verify] 缺少契约文件：$CONTRACT" >&2
  exit 1
fi

echo "[verify] 契约：$CONTRACT"
echo "[verify] 运行静态契约测试（不启动栈）"

if ! go test ./internal/devstack/ -count=1 \
  -run 'TestLoadContract14Items|TestHarnessLoopProbeTargets50013|TestProcessItemsDeclareConfigOutput|TestPIDFileStem|TestDirsResolveUnderRoot|TestContractValidationRejects|TestLoadContractErrors|TestShellScriptsStayInSync' \
  ; then
  echo "[verify] 契约校验失败" >&2
  exit 1
fi

echo "[verify] 契约校验通过：14 项、端口唯一、健康口径与脚本一致"