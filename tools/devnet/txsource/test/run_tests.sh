#!/usr/bin/env bash
# run_tests.sh —— txsource 调度器单测（mock RPC 桩，不依赖真链）
#
# 覆盖：
#   T1 段表纯函数：段边界检测（边界前/边界/边界后）、激活块号（含 delta 奇数偏移 ceil）
#   T2 全段通过：每段恰好触发一轮（9 段 = bedrock + 8 fork），激活块当拍不触发、次拍触发
#   T3 中途加入：启动头之前的段标记 missed 不补轮，仅当前段及未来段触发
#   T4 settle 失败：safe 追不上 unsafe → settle_failed=1、退出码 1
#   T5 插件失败：插件退出码非 0 → rounds_failed=1、退出码 1
#   T6 target-blocks 退出：已达目标块数则收尾退出（未触发段不再等）
#
# 桩 = test/rpc_stub.py（假 JSON-RPC：按事件脚本应答，eth_blockNumber / optimism_syncStatus
# 每次轮询推进事件指针，最后一个事件常驻重复 —— 轮询序列与脚本条目一一对应）。
set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
RUN="$DIR/../run.sh"
STUB="$DIR/rpc_stub.py"
WORK="$(mktemp -d /tmp/txsource-test.XXXXXX)"
STUB_PID=""
PASS=0; FAIL=0

cleanup() { [ -n "$STUB_PID" ] && kill "$STUB_PID" 2>/dev/null || true; }
trap cleanup EXIT

ok()   { PASS=$((PASS+1)); printf '  PASS: %s\n' "$*"; }
bad()  { FAIL=$((FAIL+1)); printf '  FAIL: %s\n' "$*"; }
check() { # $1=desc $2=expected $3=actual
  if [ "$2" = "$3" ]; then ok "$1 ($3)"; else bad "$1: want [$2] got [$3]"; fi
}

start_stub() { # $1=scenario-json-file -> sets STUB_PORT
  python3 "$STUB" "$1" >"$WORK/stub.out" 2>"$WORK/stub.log" &
  STUB_PID=$!
  local i=0
  while [ "$i" -lt 50 ]; do
    if grep -q '^PORT=' "$WORK/stub.out" 2>/dev/null; then break; fi
    sleep 0.1; i=$((i+1))
  done
  STUB_PORT="$(sed -n 's/^PORT=//p' "$WORK/stub.out")"
  [ -n "$STUB_PORT" ] || { cat "$WORK/stub.log"; echo "stub failed to start" >&2; exit 1; }
}

stop_stub() { [ -n "$STUB_PID" ] && { kill "$STUB_PID" 2>/dev/null || true; wait "$STUB_PID" 2>/dev/null || true; }; STUB_PID=""; }

scenario() { # $@=events -> 写 $WORK/scenario.json；一条事件一个 "head[:safe:unsafe]"
  python3 - "$WORK/scenario.json" "$@" <<'PYEOF'
import json, sys
out = sys.argv[1]
events = []
for a in sys.argv[2:]:
    parts = a.split(":")
    ev = {"head": int(parts[0])}
    if len(parts) >= 3:
        ev["safe"], ev["unsafe"] = int(parts[1]), int(parts[2])
    events.append(ev)
json.dump({"ts0": 1000, "block_time": 2, "chain_id": 901, "events": events}, open(out, "w"))
PYEOF
}

run_runner() { # $@=extra runner args -> exits with runner rc; stdout JSON -> $WORK/summary.out
  local outdir="$1"; shift
  set +e
  "$RUN" --plugin "$DIR/fake_plugin.sh" --devnet-toml "" \
    --rollup-json "$DIR/fixtures/rollup_stub.json" \
    --l2-rpc "http://127.0.0.1:$STUB_PORT" --opnode-rpc "http://127.0.0.1:$STUB_PORT" \
    --no-fund --poll-interval 0.05 --settle-timeout "${SETTLE_TIMEOUT:-5}" \
    --run-timeout 60 --out-dir "$outdir" "$@" \
    >"$WORK/runner.out" 2>"$WORK/runner.log"
  RUNNER_RC=$?
  set -e
}

echo "== T1 段表纯函数（边界检测 / 激活块号） =="
(
  # 先 source 再赋值：run.sh 顶部默认值段落在 source 时执行，会覆盖预先 export 的变量
  source "$RUN"
  ROLLUP_JSON="$DIR/fixtures/rollup_stub.json"; SEGS="$WORK/t1.segs"; SEGMENTS_EXPECT=9
  load_segments
  check "segment count" 9 "$(segment_count)"
  check "last segment" jovian "$(last_segment)"
  # 边界检测：边界前一段 / 边界即切 / 边界后保持；激活块号（delta 奇数偏移 ceil → 938）
  check "bedrock before canyon"   bedrock "$(segment_for_ts 2249)"
  check "canyon at boundary"      canyon  "$(segment_for_ts 2250)"
  check "canyon after boundary"   canyon  "$(segment_for_ts 2251)"
  check "delta at 2875(odd ts)"   delta   "$(segment_for_ts 2875)"
  check "ecotone at 3500"         ecotone "$(segment_for_ts 3500)"
  check "fjord at 4750"           fjord   "$(segment_for_ts 4750)"
  check "granite at 6000"         granite "$(segment_for_ts 6000)"
  check "holocene at 7250"        holocene "$(segment_for_ts 7250)"
  check "isthmus at 8500"         isthmus "$(segment_for_ts 8500)"
  check "jovian at 9750"          jovian  "$(segment_for_ts 9750)"
  check "canyon activation"   625  "$(segment_act canyon)"
  check "delta activation"    938  "$(segment_act delta)"
  check "ecotone activation"  1250 "$(segment_act ecotone)"
  check "fjord activation"    1875 "$(segment_act fjord)"
  check "granite activation"  2500 "$(segment_act granite)"
  check "holocene activation" 3125 "$(segment_act holocene)"
  check "isthmus activation"  3750 "$(segment_act isthmus)"
  check "jovian activation"   4375 "$(segment_act jovian)"
  check "bedrock activation"  0    "$(segment_act bedrock)"
) && echo "T1 done" || bad "T1 subshell crashed"

echo "== T2 全段通过：每段恰好一轮 + 激活块跳过 =="
# 事件序列：blockNumber 轮询与每轮 settle 的 syncStatus 轮询逐条消耗（见文件头说明）
scenario \
  0        \
  625  625:625:625 \
  626  626:626:626 \
  1250 1250:1250:1250 \
  1251 1251:1251:1251 \
  1875 \
  1876 1876:1876:1876 \
  2500 \
  2501 2501:2501:2501 \
  3125 \
  3126 3126:3126:3126 \
  3750 \
  3751 3751:3751:3751 \
  4375 \
  4376 4400:4400:4400
start_stub "$WORK/scenario.json"
TXSOURCE_PLUGIN_LOG="$WORK/t2.log"; : >"$TXSOURCE_PLUGIN_LOG"
export TXSOURCE_PLUGIN_LOG
run_runner "$WORK/t2" --segments-expect 9
check "T2 runner exit" 0 "$RUNNER_RC"
want_log="bedrock/1
canyon/2
delta/3
ecotone/4
fjord/5
granite/6
holocene/7
isthmus/8
jovian/9"
if [ "$want_log" = "$(cat "$TXSOURCE_PLUGIN_LOG")" ]; then
  ok "T2 plugin fired exactly once per segment, in order, rounds 1..9"
else
  bad "T2 plugin log mismatch:
$(cat "$TXSOURCE_PLUGIN_LOG")"
fi
check "T2 rounds_count"   9 "$(jq -r .rounds_count "$WORK/t2/summary.json")"
check "T2 rounds_failed"  0 "$(jq -r .rounds_failed "$WORK/t2/summary.json")"
check "T2 settle_failed"  0 "$(jq -r .settle_failed "$WORK/t2/summary.json")"
check "T2 totals.tx_total" 9 "$(jq -r .totals.tx_total "$WORK/t2/summary.json")"
stop_stub

echo "== T3 中途加入：启动头之前的段 missed 不补轮 =="
scenario \
  3000 3000:3000:3000 \
  3125 \
  3126 3126:3126:3126 \
  3750 \
  3751 3751:3751:3751 \
  4375 \
  4376 4400:4400:4400
start_stub "$WORK/scenario.json"
export TXSOURCE_PLUGIN_LOG="$WORK/t3.log"; : >"$TXSOURCE_PLUGIN_LOG"
run_runner "$WORK/t3"
check "T3 runner exit" 0 "$RUNNER_RC"
check "T3 fired rounds (当前段及未来段)" "granite/1
holocene/2
isthmus/3
jovian/4" "$(cat "$TXSOURCE_PLUGIN_LOG")"
check "T3 missed warnings" 5 "$(grep -c 'already in chain history' "$WORK/runner.log")"
check "T3 rounds_count" 4 "$(jq -r .rounds_count "$WORK/t3/summary.json")"
stop_stub

echo "== T4 settle 失败（safe 追不上 unsafe） =="
scenario \
  100 100:0:200
start_stub "$WORK/scenario.json"
export TXSOURCE_PLUGIN_LOG="$WORK/t4.log"; : >"$TXSOURCE_PLUGIN_LOG"
SETTLE_TIMEOUT=1 run_runner "$WORK/t4" --target-blocks 100
check "T4 runner exit (settle 失败 → 1)" 1 "$RUNNER_RC"
check "T4 settle_failed" 1 "$(jq -r .settle_failed "$WORK/t4/summary.json")"
check "T4 rounds fired" "bedrock/1" "$(cat "$TXSOURCE_PLUGIN_LOG")"
stop_stub

echo "== T5 插件失败（退出码非 0） =="
scenario \
  100 100:100:100
start_stub "$WORK/scenario.json"
export TXSOURCE_PLUGIN_LOG="$WORK/t5.log"; : >"$TXSOURCE_PLUGIN_LOG"
FAKE_PLUGIN_FAIL=3 run_runner "$WORK/t5" --target-blocks 100
check "T5 runner exit (插件失败 → 1)" 1 "$RUNNER_RC"
check "T5 rounds_failed" 1 "$(jq -r .rounds_failed "$WORK/t5/summary.json")"
check "T5 plugin_rc recorded" 3 "$(jq -r '.rounds[0].plugin_rc' "$WORK/t5/summary.json")"
stop_stub

echo "== T6 target-blocks 退出（到达即收尾） =="
scenario \
  50 50:50:50 \
  700 700:700:700
start_stub "$WORK/scenario.json"
export TXSOURCE_PLUGIN_LOG="$WORK/t6.log"; : >"$TXSOURCE_PLUGIN_LOG"
run_runner "$WORK/t6" --target-blocks 700
check "T6 runner exit" 0 "$RUNNER_RC"
# 语义：退出判定在触发判定之后 —— 头到达 700 时 canyon 已 pending 且激活块已过，仍触发该轮
check "T6 rounds until target" "bedrock/1
canyon/2" "$(cat "$TXSOURCE_PLUGIN_LOG")"
stop_stub

echo
echo "RESULT: PASS=$PASS FAIL=$FAIL (artifacts in $WORK)"
[ "$FAIL" -eq 0 ]
