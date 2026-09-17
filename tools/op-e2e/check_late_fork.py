#!/usr/bin/env python3
"""Late-fork activation boundary assertions (op-e2e batch-2, S1/S8-S9).

FISCO equivalent of op-e2e actions TestHoloceneLateActivationAndReset on this
lane: Isthmus baseline + LATE Jovian. Asserts, against a running node:
  1. A block exists strictly before jovian_time and one at/after it.
  2. Pre-activation blocks carry the 9-byte Holocene extraData form (0x00...).
  3. At/after activation blocks carry the 17-byte Jovian form (0x01...) — the
     boundary block itself flips shape (S8/S9: the 1559-parameter source switches
     from chain-config to extraData at Holocene+ shapes).
  4. The activation block (first at/after jovian_time) is deposits-only: tx[0]
     is type 0x7e and there are no user transactions in it.
  5. The chain continues past the activation (head > activation block).

Usage: check_late_fork.py --rpc URL --rollup rollup.json
Exit 0 = all assertions hold; nonzero with a named failure otherwise.
"""
import argparse
import json
import sys
import urllib.request


def rpc(url, method, params):
    req = urllib.request.Request(
        url, data=json.dumps({"jsonrpc": "2.0", "id": 1, "method": method,
                              "params": params}).encode(),
        headers={"Content-Type": "application/json"})
    return json.loads(urllib.request.urlopen(req, timeout=10).read())["result"]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--rpc", required=True)
    ap.add_argument("--rollup", required=True)
    args = ap.parse_args()

    rollup = json.load(open(args.rollup))
    jovian = rollup.get("jovian_time")
    if not isinstance(jovian, int):
        print(f"FAIL rollup: jovian_time missing/not int: {jovian!r}")
        return 1

    head = rpc(args.rpc, "eth_getBlockByNumber", ["latest", False])
    head_n = int(head["number"], 16)
    head_t = int(head["timestamp"], 16)
    if head_t < jovian:
        print(f"FAIL head (block {head_n}, ts {head_t}) has not crossed the "
              f"activation (jovian_time {jovian}) — let the run continue")
        return 1

    # Walk back to find the activation block: first block with ts >= jovian.
    act = None
    pre = None
    for n in range(head_n, max(head_n - 400, 0), -1):
        b = rpc(args.rpc, "eth_getBlockByNumber", [hex(n), False])
        t = int(b["timestamp"], 16)
        if t >= jovian:
            act = b
        else:
            pre = b
            break
    if act is None or pre is None:
        print("FAIL could not locate the activation/pre pair by timestamp walk")
        return 1

    def shape(b):
        ed = b.get("extraData", "0x")[2:]
        return (len(ed) // 2, ed[0:2] if ed else "")

    fails = []
    pre_len, pre_ver = shape(pre)
    if not (pre_len == 9 and pre_ver == "00"):
        fails.append(f"pre-activation block {int(pre['number'],16)}: extraData "
                     f"{pre_len}B v{pre_ver}, want 9B v00 (Holocene form)")
    act_len, act_ver = shape(act)
    act_n = int(act["number"], 16)
    if not (act_len == 17 and act_ver == "01"):
        fails.append(f"activation block {act_n}: extraData {act_len}B v{act_ver}, "
                     f"want 17B v01 (Jovian form)")
    # The block AFTER activation must keep the Jovian form (shape persists).
    post = rpc(args.rpc, "eth_getBlockByNumber", [hex(act_n + 1), False])
    if post:
        post_len, post_ver = shape(post)
        if not (post_len == 17 and post_ver == "01"):
            fails.append(f"post-activation block {act_n+1}: {post_len}B v{post_ver}")

    # Deposits-only at the activation block: EVERY tx is deposit-type (0x7e) —
    # the L1 attributes deposit plus the fork's network-upgrade deposits (Jovian
    # injects exactly 5 upstream). "Deposits-only" means no USER transactions,
    # not "exactly one tx": observed live as 1+5=6 all-0x7e.
    act_full = rpc(args.rpc, "eth_getBlockByNumber", [hex(act_n), True])
    txs = act_full.get("transactions", [])
    user = [t for t in txs if t.get("type") != "0x7e"]
    if not txs or txs[0].get("type") != "0x7e" or user:
        fails.append(f"activation block {act_n}: not deposits-only "
                     f"({len(txs)} txs, {len(user)} non-deposit, tx[0] type "
                     f"{txs[0].get('type') if txs else 'none'})")

    if head_n <= act_n:
        fails.append(f"head {head_n} did not advance past activation {act_n}")

    if fails:
        for f in fails:
            print("FAIL", f)
        return 1
    print(f"OK late-fork boundary: pre@{int(pre['number'],16)} 9B v00 -> "
          f"act@{act_n} deposits-only 17B v01 -> head@{head_n} (jovian_time {jovian})")
    return 0


if __name__ == "__main__":
    sys.exit(main())
