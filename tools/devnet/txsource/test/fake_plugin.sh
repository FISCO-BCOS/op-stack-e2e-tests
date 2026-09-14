#!/usr/bin/env bash
# fake_plugin.sh —— 单测桩插件：记录被调用参数、返回合法报告，不发生任何交易
set -euo pipefail
SEG="?" ROUND="?"
while [ $# -gt 0 ]; do
  case "$1" in
    --segment) SEG="${2:?}"; shift 2 ;;
    --round)   ROUND="${2:?}"; shift 2 ;;
    *) shift ;;
  esac
done
if [ -n "${FAKE_PLUGIN_FAIL:-}" ]; then
  echo "fake plugin instructed to fail" >&2
  echo '{"tx_total":1,"included":0,"reverted":0,"by_type":{}}'
  exit "${FAKE_PLUGIN_FAIL}"
fi
printf '%s/%s\n' "$SEG" "$ROUND" >>"${TXSOURCE_PLUGIN_LOG:?}"
echo "{\"tx_total\":1,\"included\":1,\"reverted\":0,\"by_type\":{\"noop\":1}}"
