package main

// P2-B transaction-type diversity probes (design ladder differential, stage B).
//
// The ladder's non-deposit txs used to be eip1559-only (plus one %100 CREATE).
// This file injects, ONCE PER FORK SEGMENT on the segment probe blocks
// (ladderCreateCallProbeBlocks, P2-A2), one transaction of each remaining
// envelope type so every fork boundary is crossed with all four shapes:
//
//   - accesslist (type 0x01, EIP-2930): a value-0 transfer to recA whose
//     access list pre-declares recA and one storage-churn slot. The declared
//     entries pay intrinsic gas (2400 per address + 1900 per key), so receipt
//     gasUsed proves the 0x01 decode + intrinsic accounting end-to-end without
//     changing any state (warming is execution-scoped; nothing is written).
//   - legacy (type 0x00): the legacyTransferTx shape (EIP-155 protected
//     signature, sole gasPrice 2 gwei), 21000-gas plain transfer -- the exact
//     legacy sig/RLP encoding path, in-chain.
//   - setcode (type 0x04, EIP-7702): ONLY while Prague (= Isthmus) is active
//     at the block. Sponsor key 1 sends to the deterministic authority EOA
//     (key 4), whose authorization delegates it to the P2-A logs predeploy
//     (ladderLogsProbeAddr); a follow-up call executes the delegated runtime
//     in the authority's context (one 3-topic log). The authority is absent at
//     genesis, so the FIRST delegation block creates it (nonce 0->1, code ->
//     0xef0100||ladderLogsProbeAddr) and every later block must declare it a
//     postState candidate (emitPostState completeness check) -- inject... returns
//     true on a delegation and the caller flips the candidate bookkeeping.
//     Auth nonces chain across segments (0 in the first Prague segment, 1 in
//     the second, ...) via the delegationNonce argument.
//
// All txs are appended AFTER every pre-existing injection, so existing tx
// positions, nonces and receipt indices are untouched. Determinism: every
// nonce is read live through nextNonce (bg.TxNonce(senderAddr)); signatures
// are RFC6979-deterministic (same construction as cases.go setcode_7702).

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/params"
)

// ladderDelegateAuthority is the 7702 delegation authority (key 4's EOA). It
// is NOT in the genesis alloc; the first delegation creates it.
var ladderDelegateAuthority = addrOfKey(4)

const (
	// 21000 intrinsic + 2400 (recA tuple) + 2400 (churn tuple) + 1900 (one key)
	// = 27700 for the access-list probe; the plain transfer adds nothing.
	ladderAccessListProbeGas = 60_000
	ladderLegacyProbeGas     = 21_000 // plain transfer: exactly intrinsic
	// setcode intrinsic: 21000 + 1x PER_EMPTY_ACCOUNT_COST (25000); the
	// delegation designator write and the empty-calldata delegated STOP fit
	// comfortably.
	ladder7702DelegateGas = 200_000
	// delegated call running the logs probe runtime (LOG3 path).
	ladder7702CallGas = 200_000
)

// accessListTransferTx builds a type-0x01 (EIP-2930) transfer with the same
// fee shape as legacyTransferTx (sole GasPrice 2 gwei >= the 1 gwei base fee).
func accessListTransferTx(key byte, nonce uint64, to common.Address, value *big.Int, gas uint64, al []outputAccessTuple) inputTx {
	k := privKey(key)
	toCopy := to
	return inputTx{
		OpType:     "accesslist",
		ChainID:    hd256(chainID),
		Nonce:      hd64(nonce),
		To:         &toCopy,
		Value:      hd256(value),
		Gas:        hd64(gas),
		GasPrice:   hdu(2_000_000_000),
		AccessList: al,
		SecretKey:  &k,
	}
}

// ladderAccessListProbeAL pre-declares the accessed recipient (recA, empty key
// list) and one storage-churn slot (slot 0 of the churn predeploy; the
// declaration only pays intrinsic -- it writes nothing).
func ladderAccessListProbeAL() []outputAccessTuple {
	return []outputAccessTuple{
		{Address: recA, StorageKeys: []common.Hash{}},
		{Address: ladderStorageProbeAddr, StorageKeys: []common.Hash{{}}},
	}
}

// injectLadderTxTypeProbes appends the per-segment tx-type probes (P2-B) after
// every pre-existing injection. nextNonce returns bg.TxNonce(senderAddr) at
// injection time so the key-1 nonces chain deterministically. delegationNonce
// is the running count of prior 7702 delegations on the chain (the authority
// EIP-7702 nonce must equal the authority account's live nonce). Returns true
// iff a delegation tx was injected (Prague active at blockTime); the caller
// must then keep ladderDelegateAuthority in EVERY later block's candidate set
// (the delegated account exists in the trie from that block on, so the
// postState completeness check requires it).
func injectLadderTxTypeProbes(add func(inputTx), nextNonce func() uint64,
	cfg *params.ChainConfig, blockTime uint64, delegationNonce uint64) bool {

	// 1) EIP-2930 (type 0x01): declared-access transfer, no state change.
	add(accessListTransferTx(1, nextNonce(), recA, big.NewInt(0),
		ladderAccessListProbeGas, ladderAccessListProbeAL()))

	// 2) legacy (type 0x00): plain 21000-gas transfer (EIP-155 sig path).
	add(legacyTransferTx(1, nextNonce(), recA, big.NewInt(0), ladderLegacyProbeGas, nil))

	// 3) EIP-7702 (type 0x04): Prague (= Isthmus) only. Regolith..Holocene
	//    segments reject the 0x04 envelope (op-geth ErrTxTypeNotSupported), so
	//    they stay 2930/legacy-only by design.
	if !cfg.IsPrague(big.NewInt(0), blockTime) {
		return false
	}
	authKey := privKey(4)
	sponsorKey := privKey(1)
	toCopy := ladderDelegateAuthority
	add(inputTx{
		OpType:               "setcode",
		ChainID:              hd256(chainID),
		Nonce:                hd64(nextNonce()),
		To:                   &toCopy,
		Value:                hd256(big.NewInt(0)),
		Gas:                  hd64(ladder7702DelegateGas),
		MaxFeePerGas:         hdu(2_000_000_000),
		MaxPriorityFeePerGas: hdu(100_000_000),
		SecretKey:            &sponsorKey,
		OpAuthorizations: []inputAuthorization{{
			ChainID:       hd256(chainID),
			Address:       ladderLogsProbeAddr,
			Nonce:         math.HexOrDecimal64(delegationNonce),
			AuthSecretKey: authKey,
		}},
	})
	// Delegated call: the logs probe runtime executes in the authority's
	// context and emits its 3-topic log FROM the authority address.
	add(transferTx(1, nextNonce(), ladderDelegateAuthority, big.NewInt(0),
		ladder7702CallGas, ladderLogsProbeSelectors[0]))
	return true
}
