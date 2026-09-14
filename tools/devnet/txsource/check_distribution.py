#!/usr/bin/env python3
"""check_distribution.py —— 核查用户交易（非 L1-attributes）在各 fork 段的分布（Task 4-C）。

    ./check_distribution.py --rollup /tmp/opdevnet/artifacts/rollup.json --rpc http://127.0.0.1:9545 \
                            [--funding-addr 0x..] [--out summary.json]

判定（与 README Task1/Task3 定案一致）：
  - 段表权威 = rollup.json：bedrock（genesis.l2_time 起）+ 每个 <fork>_time；
    块 ts 落在 [seg_start, next_start) 即属该段。
  - L1-attributes 交易（op-node 注入的派生交易，非用户交易）识别：
      type 0x7E（deposit，信封 0x7ef150 内层）或 from == 0xdead…0001（attributes 结算方）；
      其余（含 to=L1Block 预部署的 L1-info）均按 from=dead 排除 —— 即「非 attributes」=
      from != 0xdead000000000000000000000000000000000001 且 type != 0x7E。
  - 逐段输出：块数、attributes 交易数、用户交易数、用户交易的 from 分布（可选过滤
    --funding-addr 只看资金账户）、按 type 细分。退出码：0 = 用户交易落在 >=2 个段；
    1 = 全部用户交易只在 0-1 个段（未形成跨段覆盖）。
"""
import argparse
import json
import sys
import time
import urllib.request

DEAD = "0xdead000000000000000000000000000000000001"


def rpc(url, method, params, retries=3):
    req = urllib.request.Request(
        url,
        data=json.dumps({"jsonrpc": "2.0", "id": 1, "method": method, "params": params}).encode(),
        headers={"Content-Type": "application/json"},
    )
    last = None
    for i in range(retries):
        try:
            with urllib.request.urlopen(req, timeout=20) as resp:
                out = json.loads(resp.read())
            if "error" in out:
                raise RuntimeError(out["error"])
            return out["result"]
        except Exception as e:  # noqa: BLE001
            last = e
            time.sleep(0.5 * (i + 1))
    raise RuntimeError(f"{method} failed: {last}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--rollup", required=True)
    ap.add_argument("--rpc", required=True)
    ap.add_argument("--funding-addr", help="只统计该地址发出的用户交易（其余用户交易仍计数）")
    ap.add_argument("--out", help="把逐段明细写入 JSON 文件")
    a = ap.parse_args()

    r = json.load(open(a.rollup))
    l2t = r["genesis"]["l2_time"]
    import re
    segs = [("bedrock", l2t)]
    for k, v in sorted(r.items()):
        if re.fullmatch(r"[a-z0-9]+_time", k) and isinstance(v, int) and v > l2t and k != "block_time":
            segs.append((k[: -len("_time")], v))
    segs.sort(key=lambda x: x[1])

    head = int(rpc(a.rpc, "eth_blockNumber", []), 16)

    def seg_of(ts):
        cur = segs[0][0]
        for name, t in segs:
            if ts >= t:
                cur = name
        return cur

    rows = {name: {"blocks": 0, "attr_txs": 0, "user_txs": 0, "user_by_type": {}, "user_from": {}} for name, _ in segs}
    for n in range(1, head + 1):
        blk = rpc(a.rpc, "eth_getBlockByNumber", [hex(n), True])  # full objects：每块一次 RPC
        if not blk:
            continue
        seg = seg_of(int(blk["timestamp"], 16))
        rows[seg]["blocks"] += 1
        for tx in blk.get("transactions", []):
            frm = (tx.get("from") or "").lower()
            typ = (tx.get("type") or "0x0").lower()
            if frm == DEAD or typ == "0x7e":
                rows[seg]["attr_txs"] += 1
                continue
            rows[seg]["user_txs"] += 1
            rows[seg]["user_by_type"][typ] = rows[seg]["user_by_type"].get(typ, 0) + 1
            key = frm if not a.funding_addr or frm == a.funding_addr.lower() else "other"
            rows[seg]["user_from"][key] = rows[seg]["user_from"].get(key, 0) + 1

    print(f"{'segment':10} {'blocks':>7} {'attr_txs':>9} {'user_txs':>9}  user_from")
    total_user_segs = 0
    detail = {}
    for name, _ in segs:
        row = rows[name]
        marked = " <== " if row["user_txs"] else ""
        if row["user_txs"]:
            total_user_segs += 1
        print(f"{name:10} {row['blocks']:>7} {row['attr_txs']:>9} {row['user_txs']:>9}  {row['user_from']}{marked}")
        detail[name] = row
    summary = {"head": head, "segments": detail, "segments_with_user_txs": total_user_segs}
    if a.out:
        json.dump(summary, open(a.out, "w"), indent=1)
    print(f"RESULT: user(non-attribute) txs in {total_user_segs} segment(s) / {len(segs)}")
    sys.exit(0 if total_user_segs >= 2 else 1)


if __name__ == "__main__":
    main()
