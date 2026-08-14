#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# easyeda-agent → DeepSeek Harness (DSH) 一键接入脚本（幂等，可重复执行）
#
# 做两件事：
#   1. Skill：软链 $REPO/skills/easyeda-agent → $DSH_HOME/skills/easyeda-agent
#      （DSH skill-filesystem watcher 即时发现，无需重启）
#   2. MCP：在 $DSH_HOME/profiles/<profile>/cordis.patch.yml 注入
#      @deepseek-ai/dsh-mcp-client 实例（in-box 插件，不需要 pnpm 安装）
#      （host 插件改动需重启 dsh <profile> 才加载）
#
# 用法：
#   bash scripts/dsh-install.sh                 # profile 默认 web
#   bash scripts/dsh-install.sh --profile headless
#   DSH_HOME=/path/to/.dsh bash scripts/dsh-install.sh
#
# 前提：已安装 easyeda CLI（curl -fsSL .../install.sh | sh）且已 clone 本仓库；
#       web profile 至少启动过一次（否则先 dsh web 初始化再跑本脚本）。
# ─────────────────────────────────────────────────────────────────────────────

set -euo pipefail

# 仓库根 = 脚本所在目录的上级（跟随软链解析）
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

PROFILE="web"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --profile) PROFILE="${2:?--profile 需要参数}"; shift 2 ;;
    *) echo "未知参数: $1" >&2; exit 2 ;;
  esac
done

DSH_HOME="${DSH_HOME:-$HOME/.dsh}"
SKILL_ROOT="${DSH_HOME}/skills"
PROFILE_DIR="${DSH_HOME}/profiles/${PROFILE}"
PATCH_FILE="${PROFILE_DIR}/cordis.patch.yml"
MCP_SERVER="${REPO_ROOT}/mcp/src/server.mjs"
EASYEDA_BIN="$(command -v easyeda || true)"

echo "==> DSH home:      ${DSH_HOME}"
echo "==> profile:       ${PROFILE}"
echo "==> 仓库根:        ${REPO_ROOT}"

# ── 前置检查 ────────────────────────────────────────────────────────────────
if [[ -z "${EASYEDA_BIN}" ]]; then
  echo "!! 未找到 easyeda CLI，请先安装：" >&2
  echo "   curl -fsSL https://raw.githubusercontent.com/zhoushoujianwork/easyeda-agent/main/install.sh | sh" >&2
  exit 1
fi
if [[ ! -f "${MCP_SERVER}" ]]; then
  echo "!! 找不到 MCP server: ${MCP_SERVER}" >&2
  exit 1
fi
if [[ ! -d "${PROFILE_DIR}" ]]; then
  echo "!! profile 目录不存在: ${PROFILE_DIR}" >&2
  echo "   请先用 ${PROFILE} profile 启动过一次 dsh（如 dsh ${PROFILE}）再重跑本脚本。" >&2
  exit 1
fi

# ── 1. Skill 软链 ───────────────────────────────────────────────────────────
mkdir -p "${SKILL_ROOT}"
LINK="${SKILL_ROOT}/easyeda-agent"
if [[ -L "${LINK}" ]]; then
  TARGET="$(readlink "${LINK}")"
  if [[ "${TARGET}" != "${REPO_ROOT}/skills/easyeda-agent" ]]; then
    echo "!! skill 软链已存在但指向其它位置: ${TARGET}，更新为当前仓库" >&2
    ln -sfn "${REPO_ROOT}/skills/easyeda-agent" "${LINK}"
  else
    echo "==> skill 软链已就位（跳过）"
  fi
elif [[ -e "${LINK}" ]]; then
  echo "!! ${LINK} 已存在但不是软链，请手动处理" >&2
  exit 1
else
  ln -s "${REPO_ROOT}/skills/easyeda-agent" "${LINK}"
  echo "==> skill 已软链: ${LINK}"
fi

# ── 2. MCP patch 注入（幂等）────────────────────────────────────────────────
# 用 python3 合并：已存在 id: easyeda-mcp 则原地更新（含路径/环境变化），
# 否则追加一个独立 `- insert:` 条目。维护 marker 注释便于人工识别。
python3 - "${PATCH_FILE}" "${MCP_SERVER}" "${EASYEDA_BIN}" "${PROFILE}" <<'PYEOF'
import re, sys

patch_file, mcp_server, easyeda_bin, profile = sys.argv[1:5]

marker = "# ── easyeda-agent MCP bridge (managed by scripts/dsh-install.sh) ─────"
entry = (
    marker + "\n"
    "    - id: easyeda-mcp\n"
    "      name: '@deepseek-ai/dsh-mcp-client'\n"
    "      config:\n"
    "        serverName: easyeda\n"
    "        transport: stdio\n"
    "        command: node\n"
    "        args:\n"
    f"          - {mcp_server}\n"
    "        env:\n"
    f"          EASYEDA_BIN: {easyeda_bin}\n"
)

try:
    with open(patch_file, encoding="utf-8") as f:
        text = f.read()
except FileNotFoundError:
    text = "# dsh profile patch layer\n[]\n"

# 顶层条目边界：行首 0–2 空格后的 `- `（4+ 空格是条目内的列表项，不算边界）
BOUNDARY = r"(?=^\s{0,2}- |\Z)"
# 已存在 easyeda-mcp 条目（任意缩进，可带可选的旧 marker 注释行）→ 整块替换
pattern = re.compile(
    r"(?:^[ \t]*# ── easyeda-agent MCP bridge.*\n)?"
    r"[ \t]*- id: easyeda-mcp\b.*?" + BOUNDARY,
    re.M | re.S,
)
if pattern.search(text):
    text = pattern.sub(entry, text, count=1)
    print("==> 已更新已有 easyeda-mcp 条目")
else:
    if re.search(r"\[\s*\]\s*$", text):
        text = re.sub(r"\[\s*\]\s*$", "", text)  # 展开空数组根
    text = text.rstrip() + "\n- insert:\n" + entry
    print("==> 已注入 easyeda-mcp 条目")
    print("    注意：in-box 插件无需 pnpm 安装；若之前误装过旧版，请执行")
    print(f"    dsh plugin --profile {profile} remove @deepseek-ai/dsh-mcp-client")

with open(patch_file, "w", encoding="utf-8") as f:
    f.write(text)
PYEOF

echo "==> patch 文件:    ${PATCH_FILE}"
echo ""
echo "┌─ 下一步 ──────────────────────────────────────────────────────────┐"
echo "│ 1) skill 已被 watcher 即时发现，新会话即可用（无需重启）。       │"
echo "│ 2) MCP 工具（mcp__easyeda__easyeda_*）需重启 dsh 才加载：        │"
echo "│     找到当前 dsh 进程 kill 后，在原目录重新 dsh ${PROFILE}        │"
echo "│ 3) 验证配置合并：                                                │"
echo "│     dsh --profile ${PROFILE} --dump-config | grep -A 16 easyeda-mcp │"
echo "│ 4) 健康自检：新会话里跑 mcp__easyeda__easyeda_health             │"
echo "└───────────────────────────────────────────────────────────────────┘"
