#!/usr/bin/env bash
# =============================================================================
# 确保 opstack t8n 参考向量落地：op-geth@pin 与 optimism@pin 就位（复用或克隆）→ 跑 regen.sh。
#
# 本地与 CI 共用同一逻辑：GitHub composite action (.github/actions/opstack-t8n-regen)
# 只负责 GitHub 专属部分（actions/setup-go + actions/cache），然后委托本脚本，
# 因此「本地跑本脚本 == CI 里 composite action 的行为」。
#
# 用法
#   bash opstack-executor/tests/t8n/generator/ensure-vectors.sh
#   OPGETH=/path/to/op-geth bash .../ensure-vectors.sh   # 指定现有 op-geth checkout
#   ./.../ensure-vectors.sh                              # 脚本可执行后直接跑
#
# 环境变量
#   OPGETH           op-geth checkout 路径。缺省用 ${OPGETH_CACHE_DIR}/op-geth-<pin>
#                    （默认 ~/.cache/op-geth/op-geth-<pin>，自动克隆并钉住 pin）。
#   OPGETH_CACHE_DIR 覆盖本地缓存目录（仅当 OPGETH 未设置时生效）。
#   OPGETH_PIN       覆盖 pin；缺省从 regen.sh 提取（单一事实源）。
#   OP_NODE_REPO     optimism checkout 路径（CL 方法号 dumper 的构建树）。缺省用
#                    ${OP_NODE_CACHE_DIR}/optimism-<pin>，自动克隆并钉住 pin。
#   OP_NODE_CACHE_DIR 覆盖 optimism 缓存目录（仅当 OP_NODE_REPO 未设置时生效）。
#   OP_NODE_PIN      覆盖 pin；缺省从 regen.sh 提取。
#
# 退出码
#   0  = 向量已生成且 regen.sh 全部校验通过；非 0 = 失败（见 regen.sh 判据）。
# =============================================================================
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
REGEN="$REPO_ROOT/opstack-executor/tests/t8n/generator/regen.sh"
[ -f "$REGEN" ] || { echo "找不到 regen.sh: $REGEN" >&2; exit 1; }

PIN="${OPGETH_PIN:-$(sed -nE 's/^PIN="([0-9a-f]{40})"/\1/p' "$REGEN")}"
[ -n "$PIN" ] || { echo "无法从 regen.sh 提取 OPGETH PIN" >&2; exit 1; }
# P1: the op-node window dumper builds inside the CL reference tree, so it must be
# provisioned too -- the composite action used to set up op-geth only.
OP_NODE_PIN="${OP_NODE_PIN:-$(sed -nE 's/^OP_NODE_PIN="\$\{OP_NODE_PIN:-([0-9a-f]{40})\}".*/\1/p' "$REGEN")}"
[ -n "$OP_NODE_PIN" ] || { echo "无法从 regen.sh 提取 OP_NODE_PIN" >&2; exit 1; }

USER_OPGETH="${OPGETH:-}"   # 用户显式传入则视为自有 checkout，绝不删除
if [ -z "$USER_OPGETH" ]; then
  CACHE_ROOT="${OPGETH_CACHE_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/op-geth}"
  OPGETH="$CACHE_ROOT/op-geth-$PIN"
fi

USER_OP_NODE="${OP_NODE_REPO:-}"   # 同上：显式传入的 optimism checkout 绝不删除
if [ -z "$USER_OP_NODE" ]; then
  OP_NODE_CACHE_ROOT="${OP_NODE_CACHE_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/optimism}"
  OP_NODE_REPO="$OP_NODE_CACHE_ROOT/optimism-$OP_NODE_PIN"
fi

command -v go >/dev/null 2>&1 || { echo "需要 Go 工具链（regen.sh 要在 op-geth 模块内 go build）" >&2; exit 1; }

# 确保 $OPGETH 是一个 HEAD==pin 且干净（regen.sh 两条前置）的 checkout。
#   - 已就位且 pin 正确：清理 regen 遗留产物（cmd/opt8n-ref 目录 + 顶层二进制），
#     保证 git status 干净后复用。
#   - 未就位：浅克隆（--filter=blob:none）→ fetch 精确 SHA → checkout。
#   - 已就位但 pin 不符：
#       用户显式传入（$USER_OPGETH）→ 视为自有 checkout，直接报错，绝不删除；
#       脚本自管缓存目录 → 视为陈旧缓存，rm -rf 后重建。
# 浅取精确 SHA 会被 GitHub 以「upload-pack: not our ref」间歇拒绝（实测同一 pin：
# amd64 腿过、arm/macos 腿连拒 4 次；同 OS 的两条腿共享缓存键，冷启时双双 miss 才暴露）。
# 策略：先 3 次浅取重试（覆盖瞬时抖动），仍失败则降级为 blobless 全 ref 取（让该 SHA 可达，
# 元数据量远小于全量），最后以 cat-file 复核可达性，不可达才按原样报错退出。
fetch_pin() {  # <repo> <pin>
  local repo="$1" pin="$2" attempt
  for attempt in 1 2 3; do
    if git -C "$repo" fetch --depth 1 origin "$pin"; then
      return 0
    fi
    echo "WARNING: shallow fetch of ${pin:0:8} into $repo failed (attempt $attempt/3); retrying" >&2
    sleep 5
  done
  echo "WARNING: bare-SHA fetch refused 3x for ${pin:0:8}; falling back to a blobless ref fetch" >&2
  git -C "$repo" fetch --filter=blob:none origin
  git -C "$repo" cat-file -e "${pin}^{commit}" 2>/dev/null && return 0
  git -C "$repo" fetch origin "$pin"   # 最后的直取，失败错误码照常上抛
}

ensure_opgeth() {
  local existing=""
  [ -d "$OPGETH/.git" ] && existing="$(git -C "$OPGETH" rev-parse HEAD 2>/dev/null || true)"
  if [ -n "$existing" ] && [ "$existing" = "$PIN" ]; then
    echo "复用 op-geth@${PIN:0:8}: $OPGETH"
    git -C "$OPGETH" clean -fdx -- cmd/opt8n-ref >/dev/null 2>&1 || true
    rm -f "$OPGETH/opt8n-ref"
  elif [ -n "$existing" ] && [ -n "$USER_OPGETH" ]; then
    echo "OPGETH 指向的 checkout 在 ${existing:0:8}，需要 ${PIN:0:8}。请改用正确 pin 的 checkout，或不要设置 OPGETH 让脚本管理缓存目录。" >&2
    exit 1
  else
    mkdir -p "$(dirname "$OPGETH")"
    rm -rf "$OPGETH"
    echo "克隆 op-geth@${PIN:0:8} -> $OPGETH"
    git clone --filter=blob:none https://github.com/ethereum-optimism/op-geth "$OPGETH"
    fetch_pin "$OPGETH" "$PIN"
    git -C "$OPGETH" checkout FETCH_HEAD
  fi
}

ensure_opgeth

# 确保 $OP_NODE_REPO 是 HEAD==pin 的 optimism checkout（regen.sh 的 CL dumper 前置）。
#   - 已就位且 pin 正确：清理 regen 遗留产物后复用。
#   - 未就位：浅克隆 → fetch 精确 SHA → checkout --detach。
#   - 已就位但 pin 不符：用户显式传入则报错；脚本自管缓存目录则 rm -rf 重建。
# 注意：optimism 树可以带非 Go 的脏文件（实测 packages/contracts-bedrock 常脏）；
# regen.sh 只断言 op-node/op-service 子树干净。
ensure_op_node() {
  local existing=""
  [ -d "$OP_NODE_REPO/.git" ] && existing="$(git -C "$OP_NODE_REPO" rev-parse HEAD 2>/dev/null || true)"
  if [ -n "$existing" ] && [ "$existing" = "$OP_NODE_PIN" ]; then
    echo "复用 optimism@${OP_NODE_PIN:0:8}: $OP_NODE_REPO"
    rm -rf "$OP_NODE_REPO/cmd/opt8n-opnodewin" "$OPGETH/cmd/opt8n-ref-opnodewin" 2>/dev/null || true
    rmdir "$OP_NODE_REPO/cmd" 2>/dev/null || true
  elif [ -n "$existing" ] && [ -n "$USER_OP_NODE" ]; then
    echo "OP_NODE_REPO 指向的 checkout 在 ${existing:0:8}，需要 ${OP_NODE_PIN:0:8}。请改用正确 pin 的 checkout，或不要设置 OP_NODE_REPO 让脚本管理缓存目录。" >&2
    exit 1
  else
    mkdir -p "$(dirname "$OP_NODE_REPO")"
    rm -rf "$OP_NODE_REPO"
    echo "克隆 optimism@${OP_NODE_PIN:0:8} -> $OP_NODE_REPO"
    git clone --filter=blob:none https://github.com/ethereum-optimism/optimism.git "$OP_NODE_REPO"
    fetch_pin "$OP_NODE_REPO" "$OP_NODE_PIN"
    git -C "$OP_NODE_REPO" checkout --detach "$OP_NODE_PIN"
  fi
}

ensure_op_node
echo ">>> 运行 regen.sh（OPGETH=${OPGETH} OP_NODE_REPO=${OP_NODE_REPO}）"
OPGETH="$OPGETH" OP_NODE_REPO="$OP_NODE_REPO" bash "$REGEN"
