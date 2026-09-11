#!/usr/bin/env bash
# =============================================================================
# opstack-executor t8n 向量整批重生成仪式
#   spec rev.4 + Phase-2 线 B Task 5 预编译矩阵 + Task 6 增强语料 +
#   Task 7 三模式（corrupt/static/invalid-tx/chain）集成 + diff 源重定义 + golden 覆盖。
# =============================================================================
#
# 用途
#   op-geth 参考向量（vectors/*.json）与引擎黄金数据（golden/engine/*.golden.json）
#   的确定性整批重生成 + 可复现性校验。数据源为 generator/*.go 里的 Go 用例定义。
#
# 依赖（不满足直接 exit 1）
#   1. op-geth checkout：默认 $OPGETH=/Users/octopus/octo/code/blockchain-impl/op-geth，
#      HEAD 必须等于 pin e8800cffe53d459cde8a07c8e8f1de9d86e79e07，且工作树必须干净。
#   2. Go 工具链：脚本把 generator/ 拷进 op-geth/cmd/opt8n-ref 并 go build。
#   3. optimism checkout（P1 matrix）：默认 $OP_NODE_REPO=/Users/octopus/octo/code/optimism，
#      HEAD 必须等于 pin 76e4fad54244ec6bd07dad07e42c82a16ab5113a，且 op-node/op-service 子树干净；
#      脚本在其 cmd/opt8n-opnodewin 内 build CL 方法号 dumper。
#
# 命令
#   OPGETH=/path/to/op-geth bash opstack-executor/tests/t8n/generator/regen.sh
#   （OPGETH 缺省用默认路径；在仓库任意位置运行均可，路径由脚本自行解析）
#
# 产物（写入 opstack-executor/tests/t8n/）
#   - cases/                      逐 case 输入（瞬态，不入库，脚本不校验其字节）
#   - vectors/*.json              逐 case 参考向量 + 三模式派生（corrupt/static/invalid-tx/chain）
#   - golden/engine/*.golden.json 引擎黄金 + chained/ 链式黄金
#   - vectors/manifest.txt        注册表（幂等 append；static item 3/12 强制排除，loader 不可表达）
#   - matrix/caps.json            op-geth 反射法报出的 Engine caps（生成物，不入库）
#   - matrix/engine_api_windows.json  op-node 在 9 fork 激活点选用的方法号（生成物，不入库）
#   - matrix/{manifest.txt,SHA256SUMS}  matrix 契约（幂等生成，入库）
#
# 何时运行
#   - CI：由 composite action .github/actions/opstack-t8n-regen 在测试前自动执行
#     （主 workflow build/coverage job 已接入）。
#   - 本地：跑 opstack-executor 测试前需先手动执行本脚本一次（生成物已 .gitignore，
#     不入库）。生成的 json 会被 git 忽略，不会污染工作树。
#
# 验证判据（全部满足才 exit 0）
#   1. 每个 cases/*.in.json 都产出对应 vectors/<base>.json 与 golden/engine/<base>.golden.json，
#      缺任一即失败。
#   2. manifest 非注释非空行集合 == cases ∪ 三模式产物集合（sort + diff 对拍），
#      防孤儿向量 / 漏注册。
#   3. git diff --exit-code 只覆盖非生成契约文件（vectors/manifest.txt、vectors/*.md、
#      golden/engine/manifest.txt、golden/engine/SHA256SUMS、matrix/manifest.txt、
#      matrix/SHA256SUMS、matrix/known_deviations.json）：regen 绝不能改动它们。
#      corpus 完整性由 #2 保证；op-geth 参考无漂移由本文件头部 PIN 保证。
#   4. matrix 工件（caps.json / engine_api_windows.json）与 known_deviations.json 均存在
#      —— 判据 3 的 git diff 对缺失路径不报错，故存在性单独判。
# =============================================================================
set -euo pipefail
OPGETH="${OPGETH:-/Users/octopus/octo/code/blockchain-impl/op-geth}"
PIN="e8800cffe53d459cde8a07c8e8f1de9d86e79e07"
GEN_DIR="$(cd "$(dirname "$0")" && pwd -P)"
T8N_DIR="$(dirname "$GEN_DIR")"
REPO_ROOT="$(git -C "$GEN_DIR" rev-parse --show-toplevel)"
SCRATCH="$OPGETH/cmd/opt8n-ref"
N_CHAIN=3
# P1 matrix 工件：op-node 是 CL 选方法号的唯一权威，其引用树必须等于 pin 且干净。
OP_NODE_REPO="${OP_NODE_REPO:-/Users/octopus/octo/code/optimism}"
OP_NODE_PIN="${OP_NODE_PIN:-76e4fad54244ec6bd07dad07e42c82a16ab5113a}"
MATRIX_DIR="$T8N_DIR/matrix"

[ "$(git -C "$OPGETH" rev-parse HEAD)" = "$PIN" ] || { echo "op-geth HEAD != $PIN" >&2; exit 1; }
# ⚠️ 检查须在 opt8n-ref 二进制/目录被本脚本创建前进行：上一轮正常退出已被 cleanup trap
# rm 掉，此处只拦「有人手工把东西丢进 op-geth 工作树」。
[ -z "$(git -C "$OPGETH" status --porcelain)" ] || { echo "op-geth worktree dirty" >&2; exit 1; }

cleanup() {
  rc=$?
  rm -rf "$SCRATCH"                                            # op-geth/cmd/opt8n-ref
  rm -f "$OPGETH/opt8n-ref" "$SCRATCH-caps" "$SCRATCH-opnodewin"
  rm -rf "$OPGETH/cmd/opt8n-caps" "$OP_NODE_REPO/cmd/opt8n-opnodewin"
  rmdir "$OP_NODE_REPO/cmd" 2>/dev/null || true                # optimism 原本无 cmd/，别留空目录
  exit $rc
}
trap cleanup EXIT

rm -rf "$SCRATCH"; cp -r "$GEN_DIR" "$SCRATCH"
( cd "$OPGETH" && go build ./cmd/opt8n-ref )

"$OPGETH/opt8n-ref" --write-cases "$T8N_DIR/cases"          # 单源重发全部 case 文件（含 legacy_transfer）

# ── 逐 case 向量 + engine golden ─────────────────────────────────────────
# 动态计数（Task 7 Step 1：原 `[ "$n" -eq 77 ]` 硬计数改动态）：每个 .in.json 必须产出
# vectors/<base>.json 与 golden/engine/<base>.golden.json，缺任一 = FAILURE。
for in_json in "$T8N_DIR"/cases/*.in.json; do               # fork 在文件名/_info 内，无 --fork
  base="$(basename "$in_json" .in.json)"
  "$OPGETH/opt8n-ref" --input "$in_json" --output "$T8N_DIR/vectors/${base}.json" \
    --golden-output "$T8N_DIR/golden/engine/${base}.golden.json" \
    --op-geth-commit "$PIN"
done
missing=0
for in_json in "$T8N_DIR"/cases/*.in.json; do
  base="$(basename "$in_json" .in.json)"
  [ -f "$T8N_DIR/vectors/${base}.json" ] || { echo "missing vector ${base}.json" >&2; missing=$((missing+1)); }
  [ -f "$T8N_DIR/golden/engine/${base}.golden.json" ] || { echo "missing golden ${base}.golden.json" >&2; missing=$((missing+1)); }
done
[ "$missing" -eq 0 ] || exit 1

# ── 三模式派生向量（Task 7 Step 1）────────────────────────────────────────
# corrupt：§4a 6 字段 × isthmus/jovian 两 base → invalid_<base>_<field>.json（12）
"$OPGETH/opt8n-ref" --mode=corrupt --base isthmus_transfer_basic --out-dir "$T8N_DIR/vectors" --op-geth-commit "$PIN"
"$OPGETH/opt8n-ref" --mode=corrupt --base jovian_transfer_basic --out-dir "$T8N_DIR/vectors" --op-geth-commit "$PIN"
# static：§4c 12 项（item 3/12 生成但强制不入 manifest——GoldenSample loader 不可表达）
"$OPGETH/opt8n-ref" --mode=static --base isthmus_transfer_basic --out-dir "$T8N_DIR/vectors" --op-geth-commit "$PIN"
# invalid-tx：9 kinds × 2 forks → invalid_<fork>_<kind>.json（18）
for kind in intrinsic_gas nonce_low nonce_high insufficient_funds fee_cap_low sender_no_eoa setcode_create empty_auth_list blob; do
  "$OPGETH/opt8n-ref" --mode=invalid-tx --base "isthmus_${kind}" --out-dir "$T8N_DIR/vectors" --op-geth-commit "$PIN"
  "$OPGETH/opt8n-ref" --mode=invalid-tx --base "jovian_${kind}" --out-dir "$T8N_DIR/vectors" --op-geth-commit "$PIN"
done
# chain：线性 + fork + break（N=3）。fork/break 带 invalid_ 前缀（E2E invalid 子集消费）。
"$OPGETH/opt8n-ref" --mode="chain:${N_CHAIN}" --out-dir "$T8N_DIR/vectors" --op-geth-commit "$PIN"
"$OPGETH/opt8n-ref" --mode="chain:${N_CHAIN}:fork" --out-dir "$T8N_DIR/vectors" --op-geth-commit "$PIN"
"$OPGETH/opt8n-ref" --mode="chain:${N_CHAIN}:break" --out-dir "$T8N_DIR/vectors" --op-geth-commit "$PIN"

"$OPGETH/opt8n-ref" --chain-output-dir "$T8N_DIR/golden/engine/chained" \
  --op-geth-commit "$PIN"                                    # 链式对 golden（chainA/B + jovianChainA/B）

# ── matrix 工件（P1）：两个 dumper 各自在对应 pin 树内 build ─────────────────
# 先验 CL 引用树（HEAD == pin 且 op-node/op-service 子树干净）；该树可被其它工作改动
# 非 Go 文件（实测 packages/contracts-bedrock 常脏），故只查 Go 相关子树。
[ "$(git -C "$OP_NODE_REPO" rev-parse HEAD)" = "$OP_NODE_PIN" ] || {
  echo "op-node HEAD != $OP_NODE_PIN" >&2; exit 1; }
[ -z "$(git -C "$OP_NODE_REPO" status --porcelain -- op-node op-service)" ] || {
  echo "op-node/op-service worktree dirty; refusing to build the dumper" >&2; exit 1; }
mkdir -p "$MATRIX_DIR"

rm -rf "$OPGETH/cmd/opt8n-caps"; mkdir -p "$OPGETH/cmd/opt8n-caps"
cp "$GEN_DIR/cmd/caps/main.go" "$OPGETH/cmd/opt8n-caps/main.go"
( cd "$OPGETH" && go build -o "$SCRATCH-caps" ./cmd/opt8n-caps )
rm -rf "$OPGETH/cmd/opt8n-caps"
"$SCRATCH-caps" "$PIN" "$MATRIX_DIR"

rm -rf "$OP_NODE_REPO/cmd/opt8n-opnodewin"; mkdir -p "$OP_NODE_REPO/cmd/opt8n-opnodewin"
cp "$GEN_DIR/cmd/opnodewin/main.go" "$OP_NODE_REPO/cmd/opt8n-opnodewin/main.go"
( cd "$OP_NODE_REPO" && go build -o "$SCRATCH-opnodewin" ./cmd/opt8n-opnodewin )
rm -rf "$OP_NODE_REPO/cmd/opt8n-opnodewin"
"$SCRATCH-opnodewin" "$OP_NODE_PIN" "$GEN_DIR/matrix/input_schedule.json" "$MATRIX_DIR"

# matrix 契约（幂等生成，禁止手改）：manifest 与 SHA256SUMS 只列**生成物**的哈希；
# known_deviations.json 是人工契约（入库），出现在 manifest 里但不由本脚本重写。
python3 - "$MATRIX_DIR" <<'PYEOF'
import hashlib, pathlib, sys
d = pathlib.Path(sys.argv[1])
gen = ["caps.json", "engine_api_windows.json"]
lines = ["# matrix artifacts (generated by regen.sh; do not edit by hand)",
         "caps.json", "engine_api_windows.json", "known_deviations.json"]
(d / "manifest.txt").write_text("\n".join(lines) + "\n")
(d / "SHA256SUMS").write_text(
    "\n".join(f"{hashlib.sha256((d / n).read_bytes()).hexdigest()}  {n}" for n in gen) + "\n")
PYEOF

# ── manifest 幂等自动追加（Task 6 Step 1）─────────────────────────────────
# append-if-absent：已注册的向量（非注释非空行）不再追加；static item 3/12 强制排除。
append_if_absent() {
  local manifest="$1"; local comment="$2"; shift 2
  local pending=()
  for f in "$@"; do
    if ! grep -qxF "$f" "$manifest"; then
      pending+=("$f")
    fi
  done
  if [ "${#pending[@]}" -gt 0 ]; then
    {
      printf '\n# %s\n' "$comment"
      printf '%s\n' "${pending[@]}"
    } >> "$manifest"
  fi
}

is_unregistered_static() {  # §4c item 3/12：生成但不入 manifest
  case "$1" in
    *_static_3.json|*_static_12.json) return 0 ;;
    *) return 1 ;;
  esac
}

manifest="$T8N_DIR/vectors/manifest.txt"
# 终审 M-2：观察者定向向量（gaslimit/basefee，bothForks）单独成组 append，勿再并入
# Phase-3 注释（否则 manifest 出现重复的 Phase-3 注释行）。幂等：已注册则两段 append 均 no-op。
observer_vectors=(isthmus_deposit_basefee_observer.json isthmus_gaslimit_observer.json
                  jovian_deposit_basefee_observer.json jovian_gaslimit_observer.json)
is_observer() {
  local base="$1"
  for o in "${observer_vectors[@]}"; do
    [ "$base" = "$o" ] && return 0
  done
  return 1
}
registerable=()
for f in "$T8N_DIR"/vectors/*.json; do
  base="$(basename "$f")"
  if is_unregistered_static "$base"; then continue; fi
  if is_observer "$base"; then continue; fi
  registerable+=("$base")
done
# 确定性顺序：排序后追加（与 diff 集合比较同序）。
# 不用 readarray/mapfile（bash 4+）：macOS runner 默认 bash 3.2 没有该命令（CI exit 127）。
sorted=()
while IFS= read -r line; do sorted+=("$line"); done < <(printf '%s\n' "${registerable[@]}" | sort)
append_if_absent "$manifest" "Phase-3 enhanced corpus (Task 6): corrupt 12 + static 10 + invalid-tx 18 + chain 6 + legacy 2 = 48 vectors (idempotent; §4c item 3/12 excluded — loader 不可表达)" "${sorted[@]}"
sorted_obs=()
while IFS= read -r line; do sorted_obs+=("$line"); done < <(printf '%s\n' "${observer_vectors[@]}" | sort)
append_if_absent "$manifest" "Dual-path observer vectors (gaslimit/basefee, bothForks)" "${sorted_obs[@]}"

# ── diff 源重定义（Task 7 Step 1，审查 R10）：cases ∪ 三模式产物 == manifest ──
# cases basename 展开（.in.json → .json）∪ 派生名（corrupt/static 注册项/invalid-tx/chain）
# 与 manifest 非注释行比集合相等（防孤儿向量/漏格）。
{
  ls "$T8N_DIR"/cases/*.in.json | xargs -n1 basename | sed 's/\.in\.json$/.json/'
  printf 'invalid_isthmus_transfer_basic_%s.json\n' stateRoot gasUsed receiptsRoot parentHash extraData blockHash
  printf 'invalid_jovian_transfer_basic_%s.json\n' stateRoot gasUsed receiptsRoot parentHash extraData blockHash
  printf 'invalid_isthmus_transfer_basic_static_%d.json\n' 1 2 4 5 6 7 8 9 10
  printf 'invalid_jovian_transfer_basic_static_%d.json\n' 11
  for kind in intrinsic_gas nonce_low nonce_high insufficient_funds fee_cap_low sender_no_eoa setcode_create empty_auth_list blob; do
    printf 'invalid_isthmus_%s.json\n' "$kind"
    printf 'invalid_jovian_%s.json\n' "$kind"
  done
  printf 'isthmus_chain_%d.json\n' "$N_CHAIN"
  printf 'jovian_chain_%d.json\n' "$N_CHAIN"
  printf 'invalid_isthmus_chain_%d_fork.json\n' "$N_CHAIN"
  printf 'invalid_jovian_chain_%d_fork.json\n' "$N_CHAIN"
  printf 'invalid_isthmus_chain_%d_break.json\n' "$N_CHAIN"
  printf 'invalid_jovian_chain_%d_break.json\n' "$N_CHAIN"
} | sort > /tmp/opt8n-left.$$
grep -v '^#' "$manifest" | sed '/^$/d' | sort > /tmp/opt8n-right.$$
if ! diff /tmp/opt8n-left.$$ /tmp/opt8n-right.$$; then
  echo "cases∪modes / manifest set mismatch" >&2
  rm -f /tmp/opt8n-left.$$ /tmp/opt8n-right.$$
  exit 1
fi
rm -f /tmp/opt8n-left.$$ /tmp/opt8n-right.$$

# ── 终步：契约文件无漂移（向量 json 不入库后，字节等同判据改为只盯非生成契约）──
# 生成的 vectors/golden json 已不入库（.gitignore + CI composite action 现场重生成），
# 因此不再有「与入库字节等同」的基线。此处只保证 regen 绝不改动非生成的契约文件：
#   - vectors/manifest.txt（corpus 契约；若合法扩 corpus，regen 会 append 它 → 本 gate
#     失败，开发须按流程提交新 manifest，属预期仪式）
#   - vectors/*.md（DIVERGENCES / ANCHOR-CORRECTIONS / OP_RECEIPT_FIELDMAP，手维护）
#   - golden/engine/manifest.txt、golden/engine/SHA256SUMS（测试不读，仅契约）
# corpus 完整性由上方判据 #2（manifest 集合 == cases∪三模式产物）保证；op-geth 参考
# 无漂移由脚本头部 PIN 保证。
git -C "$REPO_ROOT" diff --exit-code -- \
  "$T8N_DIR/vectors/manifest.txt" \
  "$T8N_DIR/vectors/"*.md \
  "$T8N_DIR/golden/engine/manifest.txt" \
  "$T8N_DIR/golden/engine/SHA256SUMS" \
  "$T8N_DIR/matrix/manifest.txt" \
  "$T8N_DIR/matrix/SHA256SUMS" \
  "$T8N_DIR/matrix/known_deviations.json"

# ---- 判据 4：matrix 工件与契约（P1）----
# 注意：判据 3 的 `git diff --exit-code -- <path>` 对「不存在」的路径不报错（untracked/缺失
# 路径无 diff），因此工件与契约的**存在性**必须单独判，否则「压根没生成」会静默通过。
[ -f "$MATRIX_DIR/caps.json" ]               || { echo "matrix: caps.json missing" >&2; exit 1; }
[ -f "$MATRIX_DIR/engine_api_windows.json" ] || { echo "matrix: engine_api_windows.json missing" >&2; exit 1; }
[ -f "$MATRIX_DIR/known_deviations.json" ]   || { echo "matrix: known_deviations.json missing" >&2; exit 1; }
