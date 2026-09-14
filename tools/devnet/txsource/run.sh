#!/usr/bin/env bash
# txsource/run.sh —— 可插拔交易源执行器（P1-3，设计 §3.2 方案 A：per-segment 全量轮）
#
#   ./run.sh --plugin <可执行> [--devnet-toml <path>] [选项]
#
# 职责：
#   1. per-segment 调度：轮询 L2 头块 → 用 rollup.json 的 fork 时间表（权威生效值，不读
#      devnet.toml 的 forks 表）判定当前段 → 每段触发恰好一轮插件，直到全部段覆盖或达到
#      --target-blocks。段表 = bedrock + rollup.json 中每个 <fork>_time。
#   2. 资金桥：每轮前从 L1 富账户（anvil key0）经 OptimismPortal.depositTransaction 向插件
#      资金账户存入确定性金额。devnet intent fundDevAccounts=false，L2 侧无预富账户，
#      资金必须真实过桥（L1 存款 → 派生 → L2 余额到账后才触发插件）。
#   3. settle：每轮插件结束后等 batcher 提交 + op-node safe 追上轮末 unsafe 快照。
#
# 插件契约见同目录 README.md。日志走 stderr，stdout 只输出最终 summary JSON。
# 本文件可被 source（单测复用段表纯函数）：仅直接执行时进入 main。
set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"

# ---------- 默认参数（main 的 --flags 覆盖） ----------
PLUGIN=""
DEVNET_TOML="$DIR/../devnet.toml"
ROLLUP_JSON=""            # 缺省 = $RUNTIME/artifacts/rollup.json（桩测试可显式指定）
L2_RPC="" OPNODE_RPC="" L1_RPC=""
CHAIN_ID=""               # 缺省 = toml l2.chain_id
RICH_KEY=""               # 资金桥 L1 富账户私钥，缺省 = toml l1.private_key（anvil acct#0）
FUNDING_KEY="0xdf57089febbacf7ba0bc227dafbffa9fc08a93fdc68e1e42411a14efcf23656e"  # anvil acct#9
FUND_AMOUNT="1"           # 每轮注入 ether 数（确定性金额）
NO_FUND=0
TARGET_BLOCKS=0           # 0 = 不按块数退出，仅按全部段覆盖退出
POLL_INTERVAL="1"
SETTLE_TIMEOUT="180"
RUN_TIMEOUT="1800"
OUT_DIR=""
SEGMENTS_EXPECT=0         # >0 时校验段数（含 bedrock）
DEPLOYER_STATE=""
SEGS="" ROUNDS_JSONL=""

usage() { sed -n '2,14p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }
log() { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*" >&2; }
die() { log "FAIL: $*"; exit 1; }

# ---------- 通用 RPC（纪律同 opdevnet.sh：数值参数一律 JSON 字符串） ----------
dec() { printf '%d' "${1:-0x0}" 2>/dev/null; }   # "0x1f" -> 31
rpc() { # $1=url $2=method [$3=params-json]
  curl -s -m 10 -X POST -H 'Content-Type: application/json' \
    --data "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"$2\",\"params\":${3:-[]}}" "$1"
}
rpc_res() { rpc "$1" "$2" "${3:-[]}" | jq -r '.result // empty' 2>/dev/null; }
l2_head() { dec "$(rpc_res "$L2_RPC" eth_blockNumber)"; }
l2_block_ts() { # $1=number（dec 只吃参数不读 stdin —— 禁止管道喂 dec）
  dec "$(rpc_res "$L2_RPC" eth_getBlockByNumber "$(printf '["0x%x", false]' "$1")" | jq -r '.timestamp // empty')"
}
sync_safe_unsafe() { # -> "safe unsafe"（本构建 op-node 返回 snake_case，兼容 camelCase）
  local s safe unsafe
  s="$(rpc_res "$OPNODE_RPC" optimism_syncStatus)" || return 1
  [ -n "$s" ] || return 1
  safe="$(dec "$(printf '%s' "$s" | jq -r '.safe_l2.number // .safeL2.number // 0')")"
  unsafe="$(dec "$(printf '%s' "$s" | jq -r '.unsafe_l2.number // .unsafeL2.number // 0')")"
  printf '%d %d' "$safe" "$unsafe"
}

# ---------- 段表（设计核心：rollup.json 权威 fork 时间表） ----------
# 载入后 SEGS 为 TSV：name \t start_ts \t activation_block。bedrock 行 activation=0。
# 段起始时间或激活块已过的语义见 main_loop 内注释。可被单测 source 后直接调用。
load_segments() {
  [ -n "$ROLLUP_JSON" ] && [ -f "$ROLLUP_JSON" ] || die "rollup.json not found: $ROLLUP_JSON"
  local tmp="${SEGS}.tmp"
  if ! python3 - "$ROLLUP_JSON" >"$tmp" 2>"$tmp.err" <<'PYEOF'
import json, re, sys
r = json.load(open(sys.argv[1]))
l2t = r["genesis"]["l2_time"]
bt = int(r.get("block_time") or 2)
rows = [("bedrock", l2t, 0)]
for k, v in sorted(r.items()):
    # 只认 fork 时间：<fork>_time 且数值在 genesis 之后（排除 block_time 等同形键）
    if re.fullmatch(r"[a-z0-9]+_time", k) and isinstance(v, int) and v > l2t and k != "block_time":
        rows.append((k[: -len("_time")], v, -(-max(v - l2t, 0) // bt)))  # ceil
rows.sort(key=lambda x: x[1])
prev = -1
for name, t, act in rows:
    if t <= prev:
        print(f"fork times not strictly increasing at {name}", file=sys.stderr); sys.exit(1)
    prev = t
    print(f"{name}\t{t}\t{act}")
PYEOF
  then
    cat "$tmp.err" >&2; rm -f "$tmp" "$tmp.err"; die "failed to build segment table from $ROLLUP_JSON"
  fi
  mv "$tmp" "$SEGS"; rm -f "$tmp.err"
  local n; n="$(awk -F'\t' 'END{print NR}' "$SEGS")"
  [ "$SEGMENTS_EXPECT" -eq 0 ] || [ "$n" -eq "$SEGMENTS_EXPECT" ] || \
    die "segment count $n != expected $SEGMENTS_EXPECT"
}
segment_for_ts() { # $1=ts -> 段名（start<=ts 的最后一行）
  awk -F'\t' -v ts="$1" '$2 <= ts {n=$1} END{print (n ? n : "bedrock")}' "$SEGS"
}
segment_count() { awk -F'\t' 'END{print NR}' "$SEGS"; }
last_segment()  { awk -F'\t' 'END{print $1}' "$SEGS"; }
segment_start() { awk -F'\t' -v s="$1" '$1==s {print $2}' "$SEGS"; }
segment_act()   { awk -F'\t' -v s="$1" '$1==s {print $3}' "$SEGS"; }
wei_ge() { python3 -c "import sys; sys.exit(0 if int(sys.argv[1]) >= int(sys.argv[2]) else 1)" "$1" "$2"; }

# ---------- 资金桥（L1 depositTransaction —— 真实过桥，非 RPC 作弊） ----------
# devnet intent fundDevAccounts=false → L2 无预富账户（实测 key0/key9 L2 余额 0），
# 富账户资金只能经 OptimismPortal 存款进入 L2。portal 地址取 op-deployer state.json。
fund_plugin() { # -> stdout: 插件资金账户 L2 地址
  command -v cast >/dev/null || die "cast required for funding bridge"
  local faddr portal amt_wei
  faddr="$(cast wallet address --private-key "$FUNDING_KEY")"
  amt_wei="$(cast to-wei "$FUND_AMOUNT" ether)"
  [ -n "$DEPLOYER_STATE" ] && [ -f "$DEPLOYER_STATE" ] || \
    die "deployer state.json not found: ${DEPLOYER_STATE:-<none>} (funding bridge needs OptimismPortalProxy)"
  portal="$(python3 - "$DEPLOYER_STATE" "$CHAIN_ID" <<'PYEOF'
import json, sys
st = json.load(open(sys.argv[1]))
cid = "0x%064x" % int(sys.argv[2])
for d in st.get("opChainDeployments", []):
    if d.get("id") == cid:
        print(d["OptimismPortalProxy"]); break
PYEOF
)"
  [ -n "$portal" ] || die "OptimismPortalProxy not found in $DEPLOYER_STATE for chain $CHAIN_ID"
  log "fund: L1 depositTransaction -> $faddr amount=${FUND_AMOUNT}ether portal=$portal"
  # 注意签名：本部署 Portal2 的 gasLimit 参数是 uint64（selector e9e05c42）；写成 uint256 会
  # 得到 0xfa92670c，dispatcher 不识别 → 空 revert（实测踩坑）
  cast send "$portal" 'depositTransaction(address,uint256,uint64,bool,bytes)' \
    "$faddr" "$amt_wei" 200000 false "0x" \
    --private-key "$RICH_KEY" --rpc-url "$L1_RPC" --value "${FUND_AMOUNT}ether" --json </dev/null \
    >"$OUT_DIR/fund-tx.json" 2>"$OUT_DIR/fund.log" || die "L1 deposit tx failed (see $OUT_DIR/fund.log)"
  local status
  status="$(jq -r '.status // "0x0"' "$OUT_DIR/fund-tx.json" 2>/dev/null || echo 0x0)"
  [ "$status" = "0x1" ] || die "L1 deposit tx reverted (status=$status)"
  # 等存款经 L1 派生落进 L2（存款直接进块，不依赖 batcher）
  local deadline=$(( $(date +%s) + 120 ))
  while :; do
    local bal; bal="$(cast balance "$faddr" --rpc-url "$L2_RPC" 2>/dev/null || echo 0)"
    if wei_ge "$bal" "$amt_wei"; then
      log "fund: L2 balance confirmed for $faddr ($bal wei)"
      printf '%s' "$faddr"; return 0
    fi
    [ "$(date +%s)" -ge "$deadline" ] && die "funding not visible on L2 within 120s (bal=$bal want>=$amt_wei)"
    sleep "$POLL_INTERVAL"
  done
}

# ---------- settle（设计 §4.3：batcher 提交 → safe 追上 unsafe 才算沉降） ----------
settle_round() { # $1=round_label $2=timeout_s -> 0/1；SETTLE_SAFE/SETTLE_UNSAFE 传出
  local label="$1" target="" su safe unsafe deadline=$(( $(date +%s) + $2 ))
  SETTLE_SAFE=""; SETTLE_UNSAFE=""
  while :; do
    if su="$(sync_safe_unsafe)"; then
      safe="${su%% *}"; unsafe="${su##* }"
      [ -z "$target" ] && target="$unsafe"
      SETTLE_SAFE="$safe"; SETTLE_UNSAFE="$unsafe"
      if [ "$safe" -ge "$target" ]; then
        log "settle[$label]: OK safe=$safe (target $target)"
        return 0
      fi
    fi
    [ "$(date +%s)" -ge "$deadline" ] && {
      log "settle[$label]: TIMEOUT after ${2}s (safe=${SETTLE_SAFE:-?} target=${target:-?})"
      return 1
    }
    sleep "$POLL_INTERVAL"
  done
}

# ---------- 单轮触发 ----------
ROUND_RC=0; SETTLE_SAFE=""; SETTLE_UNSAFE=""
fire_round() { # $1=segment $2=round
  local seg="$1" round="$2" rdir rc
  rdir="$OUT_DIR/round-$(printf '%02d' "$round")-$seg"; mkdir -p "$rdir"
  log "round $round segment=$seg begin (dir $rdir)"

  if [ "$NO_FUND" -eq 0 ]; then
    local faddr
    faddr="$(fund_plugin)" || die "funding bridge failed at round $round"
    printf '%s' "$faddr" >"$rdir/funding_addr"
  else
    log "fund: skipped (--no-fund)"
  fi

  rc=0
  ( cd "$rdir" && TXSOURCE_ROUND_DIR="$rdir" TXSOURCE_OUT_DIR="$OUT_DIR" \
      "$PLUGIN" --endpoint "$L2_RPC" --chain-id "$CHAIN_ID" --funding-key "$FUNDING_KEY" \
                --segment "$seg" --round "$round" ) </dev/null \
    >"$rdir/report.json" 2>"$rdir/plugin.log" || rc=$?
  ROUND_RC=$rc

  local rep="null" tx_total=0 included=0 reverted=0
  if [ -s "$rdir/report.json" ] && jq -e . "$rdir/report.json" >/dev/null 2>&1; then
    rep="$(jq -c . "$rdir/report.json")"
    tx_total="$(jq -r '.tx_total // 0' "$rdir/report.json")"
    included="$(jq -r '.included // 0' "$rdir/report.json")"
    reverted="$(jq -r '.reverted // 0' "$rdir/report.json")"
  else
    log "WARN: round $round plugin report missing/invalid ($rdir/report.json)"
  fi

  local settle_ok=1
  settle_round "$round/$seg" "$SETTLE_TIMEOUT" || settle_ok=0

  jq -n -c --arg seg "$seg" --argjson round "$round" --argjson rc "$rc" \
    --argjson tx_total "$tx_total" --argjson included "$included" --argjson reverted "$reverted" \
    --argjson report "$rep" --argjson settle_ok "$settle_ok" \
    --argjson safe "${SETTLE_SAFE:-null}" --argjson unsafe "${SETTLE_UNSAFE:-null}" \
    '{segment:$seg, round:$round, plugin_rc:$rc, report:$report,
      tx_total:$tx_total, included:$included, reverted:$reverted,
      settle_ok:$settle_ok, safe:$safe, unsafe:$unsafe}' >>"$ROUNDS_JSONL"
  log "round $round segment=$seg done: plugin_rc=$rc tx=$tx_total included=$included reverted=$reverted settle_ok=$settle_ok"
}

# ---------- 主循环 ----------
main_loop() {
  load_segments
  local last; last="$(last_segment)"
  log "txsource start: plugin=$PLUGIN l2=$L2_RPC opnode=$OPNODE_RPC rollup=$ROLLUP_JSON"
  log "segments ($(segment_count)): $(awk -F'\t' '{printf "%s@%d(act %d) ", $1, $2, $3}' "$SEGS"); last=$last"
  : >"$ROUNDS_JSONL"

  local round=0 fired="" missed="" head ts cur i=0
  local deadline=$(( $(date +%s) + RUN_TIMEOUT ))
  while :; do
    head="$(l2_head)"
    ts="$(l2_block_ts "$head")"
    cur="$(segment_for_ts "$ts")"

    # 首轮快照：激活块已落在启动头之前的非当前段 = 已错过（链史已定型，不补轮）
    if [ "$i" -eq 0 ]; then
      local s ms m_act
      while IFS=$'\t' read -r s ms m_act; do
        if [ "$s" != "$cur" ] && [ "$ms" -le "$ts" ] && [ "$m_act" -lt "$head" ]; then
          missed="$missed $s"
          log "WARN: segment $s already in chain history at runner start (head=$head) — no round scheduled"
        fi
      done <"$SEGS"
      log "start segment=$cur head=$head ts=$ts; missed:[${missed:- }]"
    fi

    # 触发条件（激活块跳过语义）：段未触发、未错过、段起始 <= 头 ts、且头块号严格大于激活块
    # —— 插件交易不与激活块的升级交易注入混块（设计 §7.4 激活块计数保持纯净）
    local s fs fa
    while IFS=$'\t' read -r s fs fa; do
      case " $missed " in *" $s "*) continue ;; esac
      case " $fired " in *" $s "*) continue ;; esac
      if [ "$fs" -le "$ts" ] && [ "$fa" -lt "$head" ]; then
        round=$((round+1))
        fired="$fired $s"
        fire_round "$s" "$round" || true
      fi
    done <"$SEGS"

    # 退出条件：全部段覆盖（最后一段已触发）或达到目标块数
    case " $fired " in *" $last "*) log "all segments covered (rounds=$round); exit"; break ;; esac
    if [ "$TARGET_BLOCKS" -gt 0 ] && [ "$head" -ge "$TARGET_BLOCKS" ]; then
      log "target blocks reached (head=$head >= $TARGET_BLOCKS); rounds=$round"; break
    fi
    [ "$(date +%s)" -ge "$deadline" ] && die "run timeout after ${RUN_TIMEOUT}s (head=$head rounds=$round)"
    sleep "$POLL_INTERVAL"
    i=$((i+1))
  done
}

main() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --plugin)          PLUGIN="${2:?}"; shift 2 ;;
      --devnet-toml)     DEVNET_TOML="${2-}"; shift 2 ;;   # 允许显式空串（桩测试跳过 toml）
      --rollup-json)     ROLLUP_JSON="${2:?}"; shift 2 ;;
      --l2-rpc)          L2_RPC="${2:?}"; shift 2 ;;
      --opnode-rpc)      OPNODE_RPC="${2:?}"; shift 2 ;;
      --l1-rpc)          L1_RPC="${2:?}"; shift 2 ;;
      --chain-id)        CHAIN_ID="${2:?}"; shift 2 ;;
      --rich-key)        RICH_KEY="${2:?}"; shift 2 ;;
      --funding-key)     FUNDING_KEY="${2:?}"; shift 2 ;;
      --funding-amount)  FUND_AMOUNT="${2:?}"; shift 2 ;;
      --no-fund)         NO_FUND=1; shift ;;
      --target-blocks)   TARGET_BLOCKS="${2:?}"; shift 2 ;;
      --poll-interval)   POLL_INTERVAL="${2:?}"; shift 2 ;;
      --settle-timeout)  SETTLE_TIMEOUT="${2:?}"; shift 2 ;;
      --run-timeout)     RUN_TIMEOUT="${2:?}"; shift 2 ;;
      --out-dir)         OUT_DIR="${2:?}"; shift 2 ;;
      --segments-expect) SEGMENTS_EXPECT="${2:?}"; shift 2 ;;
      -h|--help)         usage ;;
      *) echo "unknown arg: $1" >&2; usage ;;
    esac
  done

  # ---------- 配置解析（awk 解析器与 opdevnet.sh 同源，仅支持平面 key = value 格式） ----------
  # 端点/端口从 toml 读（任务书：端点/端口从 toml+status 读取）；
  # fork 时间表从 rollup.json 读（impl 计划：rollup.json 是权威生效值，不读 toml 的 forks 表）
  if [ -n "$DEVNET_TOML" ] && [ -f "$DEVNET_TOML" ]; then
    local RUNTIME
    RUNTIME="$(toml_get meta runtime_dir)"
    [ -z "$L2_RPC" ]     && L2_RPC="http://127.0.0.1:$(toml_get l2.ports http)"
    [ -z "$OPNODE_RPC" ] && OPNODE_RPC="http://127.0.0.1:$(toml_get l2.ports opnode_rpc)"
    [ -z "$L1_RPC" ]     && L1_RPC="http://127.0.0.1:$(toml_get l1.ports rpc)"
    [ -z "$CHAIN_ID" ]   && CHAIN_ID="$(toml_get l2 chain_id)"
    [ -z "$RICH_KEY" ]   && RICH_KEY="$(toml_get l1 private_key)"
    [ -z "$ROLLUP_JSON" ] && ROLLUP_JSON="$RUNTIME/artifacts/rollup.json"
    DEPLOYER_STATE="$RUNTIME/run/deployer/state.json"
  else
    [ -n "$ROLLUP_JSON" ] && [ -n "$L2_RPC" ] && [ -n "$OPNODE_RPC" ] || \
      die "no devnet.toml: --rollup-json --l2-rpc --opnode-rpc are all required"
    [ -n "$CHAIN_ID" ] || CHAIN_ID=901
    DEPLOYER_STATE=""
  fi
  [ -n "$PLUGIN" ] || die "--plugin is required"
  [ -x "$PLUGIN" ] || die "plugin not executable: $PLUGIN"
  # 解析为绝对路径：轮目录切换后仍可执行（相对路径曾致 plugin_rc=127，实测踩坑）
  PLUGIN="$(cd "$(dirname "$PLUGIN")" && pwd)/$(basename "$PLUGIN")"
  command -v jq >/dev/null || die "jq required"
  command -v python3 >/dev/null || die "python3 required"

  [ -n "$OUT_DIR" ] || OUT_DIR="/tmp/txsource/$(date +%Y%m%d-%H%M%S)"
  mkdir -p "$OUT_DIR"
  SEGS="$OUT_DIR/segments.tsv"
  ROUNDS_JSONL="$OUT_DIR/rounds.jsonl"

  main_loop

  # ---------- 汇总（stdout 仅此一份 JSON；任何轮失败或 settle 失败 → 退出码 1） ----------
  python3 - "$OUT_DIR" "$ROUNDS_JSONL" <<'PYEOF'
import json, sys
out, path = sys.argv[1], sys.argv[2]
rounds = [json.loads(l) for l in open(path) if l.strip()]
def acc(key): return sum(r.get(key, 0) for r in rounds)
summary = {
    "runner": "txsource",
    "out_dir": out,
    "rounds_count": len(rounds),
    "rounds_failed": sum(1 for r in rounds if r["plugin_rc"] != 0),
    "settle_failed": sum(1 for r in rounds if not r["settle_ok"]),
    "totals": {k: acc(k) for k in ("tx_total", "included", "reverted")},
    "rounds": rounds,
}
json.dump(summary, open(f"{out}/summary.json", "w"), indent=2)
print(json.dumps(summary))
sys.exit(1 if summary["rounds_failed"] or summary["settle_failed"] else 0)
PYEOF
}

# toml_get 定义放 main 之后亦可（bash 运行时解析），但为可读性放此：
toml_get() { # $1=section $2=key —— 与 opdevnet.sh 同源的平面 toml 解析
  awk -v sec="$1" -v key="$2" '
    { sub(/#.*/, ""); sub(/[ \t]+$/, "") }
    /^[ \t]*\[/ { cur=$0; gsub(/[][]/,"",cur); gsub(/[ \t]/,"",cur); next }
    cur == sec && NF >= 3 {
      k=$1; sub(/^[^=]*=[ \t]*/,""); v=$0
      if (k == key) { gsub(/^"|"$/,"",v); print v; exit }
    }' "$DEVNET_TOML"
}

if [ "${BASH_SOURCE[0]-}" = "$0" ]; then
  main "$@"
fi
