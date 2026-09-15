#!/usr/bin/env python3
"""check_activations.py —— devnet 激活块升级交易计数检查（Task 1 分析脚本的生产化）。

用法:
    check_activations.py --rollup artifacts/rollup.json --rpc http://127.0.0.1:9545

逻辑（Task 1 实测定案，README.md「定案结论 3」）:
  - 激活块 = 链上第一个 timestamp >= fork 边界时间的块；边界 = rollup.json 的 *_time
    （= genesis.l2_time + 阶梯偏移）。
  - 逐激活块统计 type-0x7E（deposit）交易，区分 L1-attributes（from=0xdead…0001、
    to=L1Block 预部署 0x420…0015）与升级交易，升级交易数与
    op-node/rollup/derive/attributes.go 的注入分支一一对应:
      Ecotone 6 / Fjord 3 / Isthmus 8 / Jovian 5（Jovian = DAFootprint 3 + OperatorFeeFix 2）
      Canyon / Delta / Granite / Holocene = 0（Delta 在 attributes.go 无注入分支）
  - 顺带断言: 每个激活块的父块 timestamp 严格 < 边界（激活块确为第一个越过边界的块）。

退出码: 0=全部命中; 1=计数与预期不符或边界校验失败; 2=链尚未抵达全部边界/rollup 缺字段;
       3=RPC 不可用（基础设施故障，与语义校验失败区分 —— robustness 审计 8.1）
"""
import argparse
import json
import sys
import time
import urllib.error
import urllib.request

# attributes.go 注入分支的源码定案（不要凭链上结果改这张表；链不匹配说明链有问题）
EXPECTED_UPGRADE = {
    "canyon": 0,
    "delta": 0,      # Delta 无注入分支（非标准 fork，见 README 定案结论 2）
    "ecotone": 6,
    "fjord": 3,
    "granite": 0,
    "holocene": 0,
    "isthmus": 8,
    "jovian": 5,
}
FORK_ORDER = ["canyon", "delta", "ecotone", "fjord", "granite", "holocene", "isthmus", "jovian"]

L1INFO_FROM = "0xdeaddeaddeaddeaddeaddeaddeaddeaddead0001"
L1INFO_TO = "0x4200000000000000000000000000000000000015"


def rpc(url, method, params, retries=3, timeout=20):
    """JSON-RPC with bounded retries on transport errors; final failure -> SystemExit(3).

    旧版无重试且让 URLError 裸 traceback、以退出码 1 混入「计数不符」语义（审计 8.1）。
    """
    req = urllib.request.Request(
        url,
        json.dumps({"jsonrpc":"2.0","id":1,"method":method,"params":params}).encode(),
        {"Content-Type": "application/json"},
    )
    last = None
    for i in range(retries):
        try:
            with urllib.request.urlopen(req, timeout=timeout) as r:
                out = json.load(r)
            if "error" in out:
                raise RuntimeError(f"rpc error {out['error'].get('code')}: {out['error'].get('message')}")
            return out["result"]
        except Exception as e:  # noqa: BLE001 — transport/RPC 错误统一重试后上抛
            last = e
            time.sleep(0.5 * (i + 1))
    print(f"ERROR: RPC unavailable at {url} ({method} failed after {retries} attempts): {last}\n"
          f"       is the devnet stack up?  opdevnet.sh status", file=sys.stderr)
    sys.exit(3)


def block_by_number(url, n):
    return rpc(url, "eth_getBlockByNumber", [hex(n), True])


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--rollup", required=True, help="op-deployer inspect rollup 产物")
    ap.add_argument("--rpc", required=True, help="L2 geth HTTP RPC（9545）")
    args = ap.parse_args()

    try:
        with open(args.rollup) as f:
            rollup = json.load(f)
    except (OSError, json.JSONDecodeError) as e:
        print(f"ERROR: cannot read rollup file {args.rollup}: {e}", file=sys.stderr)
        return 2
    if not isinstance(rollup.get("genesis", {}).get("l2_time"), int):
        print(f"ERROR: {args.rollup} missing/bad genesis.l2_time —— 不是 op-deployer inspect rollup 产物？",
              file=sys.stderr)
        return 2
    if not isinstance(rollup.get("block_time"), int) or rollup["block_time"] <= 0:
        print(f"ERROR: {args.rollup} missing/bad block_time", file=sys.stderr)
        return 2

    l2_time = rollup["genesis"]["l2_time"]
    block_time = rollup["block_time"]

    latest_hex = rpc(args.rpc, "eth_blockNumber", [])
    latest = int(latest_hex, 16)

    print(f"rollup: l2_time={l2_time} block_time={block_time}s l2_chain_id={rollup.get('l2_chain_id', '?')} "
          f"l2 head={latest}")
    print(f"{'fork':10} {'boundary':>11} {'actBlock':>9} {'blockTs':>11} {'7E_total':>8} {'l1info':>7} "
          f"{'upg':>4} {'expect':>6}  result")

    failures = 0
    unreachable = 0
    for name in FORK_ORDER:
        boundary = rollup.get(f"{name}_time")
        if boundary is None:
            print(f"{name:10} {'(nil=disabled)':>11}  -- skip --")
            continue
        expect_n = (boundary - l2_time + 1) // block_time
        found = None
        for n in range(max(1, expect_n - 5), expect_n + 8):
            if n > latest:
                break
            blk = block_by_number(args.rpc, n)
            if blk is None:
                continue
            if int(blk["timestamp"], 16) >= boundary:
                found = (n, blk)
                break
        if found is None:
            print(f"{name:10} {boundary:>11}  NOT REACHED (head={latest}, need~{expect_n})")
            unreachable += 1
            continue
        n, blk = found
        ts = int(blk["timestamp"], 16)
        total = 0
        l1info = 0
        upgrade = 0
        for tx in blk.get("transactions") or []:
            if tx.get("type", "0x0").lower() == "0x7e":
                total += 1
                if tx.get("from", "").lower() == L1INFO_FROM and tx.get("to", "").lower() == L1INFO_TO:
                    l1info += 1
                else:
                    upgrade += 1
        exp = EXPECTED_UPGRADE[name]
        ok = upgrade == exp
        if not ok:
            failures += 1
        # 父块校验：激活块必须是第一个 >= 边界的块
        prev = block_by_number(args.rpc, n - 1)
        prev_ts = int(prev["timestamp"], 16)
        boundary_ok = prev_ts < boundary
        if not boundary_ok:
            failures += 1
        verdict = "OK" if (ok and boundary_ok) else (
            f"FAIL(need {exp})" if not ok else "FAIL(prev-ts)"
        )
        print(f"{name:10} {boundary:>11} {n:>9} {ts:>11} {total:>8} {l1info:>7} {upgrade:>4} "
              f"{exp:>6}  {verdict}")

    if unreachable:
        print(f"\nRESULT: INCOMPLETE — {unreachable} fork boundary not yet on chain (exit 2)")
        return 2
    if failures:
        print(f"\nRESULT: FAIL — {failures} mismatch(es) vs attributes.go expectations (exit 1)")
        return 1
    print("\nRESULT: ALL MATCH — upgrade-tx counts per activation block = 6/3/8/5, others 0 (exit 0)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
