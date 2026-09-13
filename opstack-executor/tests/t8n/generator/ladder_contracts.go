package main

// P2-A contract-layer probes (design ladder differential, stage A).
//
// The ladder vector's event coverage used to be the single MessagePassed shape
// (one log, four topics) emitted by L2ToL1MessagePasser. This file adds four
// deterministic predeploys that broaden the contract-level surface:
//
//   - ladderLogsProbeAddr:    an extended logs contract (four selector-dispatched
//                             entry points: 3-topic+data Transfer shape, a
//                             multi-log tx, a zero-topic LOG0, and a 256-byte
//                             data log). Predeployed to genesis AND re-deployed
//                             in-chain via CREATE (create->call lifecycle).
//   - ladderStorageProbeAddr: a storage-churn contract. One call writes K=8
//                             slots (cold then warm per slot) and zeroes the
//                             previous probe's slot group (SSTORE clear/refund).
//   - ladderRevertProbeAddr:  always reverts with abi-encoded Error(string)
//                             reason data -> receipt status 0 + non-empty output.
//   - ladderInvalidProbeAddr: executes the INVALID (0xfe) opcode -> status 0,
//                             empty output, gasUsed == gasLimit (distinct from
//                             REVERT's partial gas).
//
// BYTE-INVARIANCE OF THE EXISTING contract_logs VECTOR. logsCode() is NOT
// touched: the vector's `pre` carries the contract's full code bytes
// (outputAccount.Code, main.go emitPre), so appending selector dispatch to
// logsCode() would change `pre.code` and split a pre-existing vector. The
// extended contract is therefore a NEW code object (ladderLogsProbeCode) that
// reuses the same LOG primitives; TestLadderLogsCodeByteStable anchors
// logsCode()'s sha256 so a future edit is caught.
//
// NO PUSH0 / MCOPY / TSTORE. The ladder's regolith segment runs BEFORE Canyon,
// and buildChainConfigSpec couples Shanghai (=PUSH0) to the canyon activation
// timestamp. The earliest probe blocks (25/50/75) are pre-Canyon, so every
// emitted opcode must be London-safe. All memory/stack zeros use PUSH1 0x00.
// CODECOPY/MSTORE/LOG*/SHR are Frontier..Constantinople and always available.

import (
	"encoding/binary"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
)

// Deterministic probe addresses. 0xc0de...0001..0008 and 0x...00e0 are already
// taken by cases.go; these four sit in the same synthetic namespace and are not
// precompiles on either side.
var (
	ladderLogsProbeAddr    = common.HexToAddress("0xc0de00000000000000000000000000000000000a")
	ladderStorageProbeAddr = common.HexToAddress("0xc0de00000000000000000000000000000000000b")
	ladderRevertProbeAddr  = common.HexToAddress("0xc0de00000000000000000000000000000000000c")
	ladderInvalidProbeAddr = common.HexToAddress("0xc0de00000000000000000000000000000000000d")
)

// ladderContractProbeAddrs are declared as ExtraCandidates on every ladder
// block. Rationale: generateLadderChain chains block i's `pre` from block i-1's
// postState, and emitPostState only emits the *candidate* accounts of the block
// that ran (main.go assembleOutput). A probe contract touched only on block 25
// would therefore vanish from block 26's postState -> block 50's `pre` would
// lose its code/storage, and the FISCO replay would read zero state where
// generation saw persistent state (storage gas/refund divergence). Keeping all
// four in every block's candidate set makes their code (+ the churn storage)
// survive the pre chain between probes.
var ladderContractProbeAddrs = []common.Address{
	ladderLogsProbeAddr, ladderStorageProbeAddr, ladderRevertProbeAddr, ladderInvalidProbeAddr,
}

const (
	ladderLogsProbeGas          = 200_000 // 4 logs calls
	ladderLogsDeployGas         = 900_000 // CREATE of ~600B runtime
	ladderChurnGas              = 500_000 // 8 cold + 8 warm writes + 8 cold clears
	ladderRevertProbeGas        = 100_000 // REVERT keeps the remainder
	ladderInvalidProbeGas       = 100_000 // INVALID burns the whole limit
	ladderContractProbeInterval = 25
	// ladderChurnSlotCount must match the unrolled loop in ladderStorageChurnCode.
	ladderChurnSlotCount  = 8
	ladderChurnSlotStride = ladderChurnSlotCount
)

var (
	ladderLogsFromAddr = common.HexToAddress("0x1111111111111111111111111111111111111111")
	ladderLogsToAddr   = common.HexToAddress("0x2222222222222222222222222222222222222222")
	ladderLogsValue    = big.NewInt(0x1234_5678)
)

// ladderLogsProbeSelectors are the four keccak-derived entry-point selectors.
// Ordering matters: index 0 is the Transfer shape (used for the in-chain
// created contract call).
var ladderLogsProbeSelectors = func() [4][]byte {
	sigs := [...]string{"ladderTransfer()", "ladderMultiLog()", "ladderZeroTopic()", "ladderLongData()"}
	var out [4][]byte
	for i, s := range sigs {
		out[i] = crypto.Keccak256([]byte(s))[:4]
	}
	return out
}()

// ---------------------------------------------------------------------
// Minimal deterministic EVM assembler (PUSH2-labelled jumps only).
// ---------------------------------------------------------------------

type ladderAsmFixup struct {
	at    int
	label string
}

type ladderAsm struct {
	code   []byte
	labels map[string]int
	fixups []ladderAsmFixup
}

func newLadderAsm() *ladderAsm { return &ladderAsm{labels: map[string]int{}} }

func (a *ladderAsm) raw(bs ...byte) *ladderAsm {
	a.code = append(a.code, bs...)
	return a
}

// push emits the shortest PUSHn for a 1..32 byte big-endian value.
func (a *ladderAsm) push(data []byte) *ladderAsm {
	if len(data) == 0 || len(data) > 32 {
		panic("ladderAsm.push: value must be 1..32 bytes")
	}
	a.code = append(a.code, byte(0x5f+len(data)))
	a.code = append(a.code, data...)
	return a
}

func (a *ladderAsm) pushUint(v uint64) *ladderAsm {
	if v == 0 {
		return a.push([]byte{0})
	}
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	i := 0
	for i < 8 && b[i] == 0 {
		i++
	}
	return a.push(b[i:])
}

func (a *ladderAsm) pushHash(h common.Hash) *ladderAsm { return a.push(h[:]) }

// pushSelector emits PUSH4 <sel> (selectors are compared as 4-byte values).
func (a *ladderAsm) pushSelector(sel []byte) *ladderAsm {
	if len(sel) != 4 {
		panic("ladderAsm.pushSelector: selector must be 4 bytes")
	}
	a.code = append(a.code, 0x63)
	a.code = append(a.code, sel...)
	return a
}

// pushLabel emits PUSH2 <placeholder> and records a fixup resolved by assemble.
func (a *ladderAsm) pushLabel(label string) *ladderAsm {
	a.code = append(a.code, 0x61, 0x00, 0x00)
	a.fixups = append(a.fixups, ladderAsmFixup{at: len(a.code) - 2, label: label})
	return a
}

func (a *ladderAsm) label(name string) *ladderAsm {
	a.labels[name] = len(a.code)
	return a
}

// jumpLabel marks a JUMPI/JUMP target: a label whose byte is a real JUMPDEST
// (the EVM rejects a jump to a non-JUMPDEST byte, burning all gas).
func (a *ladderAsm) jumpLabel(name string) *ladderAsm {
	a.labels[name] = len(a.code)
	a.code = append(a.code, 0x5b) // JUMPDEST
	return a
}

func (a *ladderAsm) assemble() []byte {
	out := make([]byte, len(a.code))
	copy(out, a.code)
	for _, f := range a.fixups {
		d, ok := a.labels[f.label]
		if !ok {
			panic("ladderAsm: unresolved label " + f.label)
		}
		if d > 0xffff {
			panic("ladderAsm: label beyond PUSH2 range: " + f.label)
		}
		out[f.at] = byte(d >> 8)
		out[f.at+1] = byte(d)
	}
	return out
}

// ---------------------------------------------------------------------
// Extended logs contract.
// ---------------------------------------------------------------------

// ladderLogsProbeCode is a selector-dispatched logs contract. Entry points
// (keccak4 of ladderTransfer()/ladderMultiLog()/ladderZeroTopic()/ladderLongData()):
//
//	0: LOG3 with 3 topics (ERC-20 Transfer signature + from + to) and a 32-byte
//	   non-empty data word.
//	1: LOG3 Transfer + LOG0 over the same word -> 2 logs in one receipt.
//	2: LOG0 with 32 zero bytes -> zero-topic log with non-empty data.
//	3: LOG1 with a 256-byte data blob copied from code -> long-data log.
//
// Unknown selectors (including empty calldata) STOP. No storage writes, so the
// account needs no ExtraStorage declaration.
func ladderLogsProbeCode() []byte {
	transferTopic := crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))
	fromTopic := common.BytesToHash(common.LeftPadBytes(ladderLogsFromAddr.Bytes(), 32))
	toTopic := common.BytesToHash(common.LeftPadBytes(ladderLogsToAddr.Bytes(), 32))
	valueWord := common.BigToHash(ladderLogsValue)
	longTopic := crypto.Keccak256Hash([]byte("ladder-long-data()"))
	longData := junkData("ladder long data", 256)

	a := newLadderAsm()
	// selector = uint32(CALLDATALOAD(0) >> 224)
	a.raw(0x60, 0x00, 0x35) // PUSH1 0; CALLDATALOAD
	a.raw(0x60, 0xe0, 0x1c) // PUSH1 0xe0; SHR
	arms := [...]struct {
		sel   []byte
		label string
	}{
		{ladderLogsProbeSelectors[0], "arm_transfer"},
		{ladderLogsProbeSelectors[1], "arm_multi"},
		{ladderLogsProbeSelectors[2], "arm_zero"},
		{ladderLogsProbeSelectors[3], "arm_long"},
	}
	for _, arm := range arms {
		a.raw(0x80)             // DUP1
		a.pushSelector(arm.sel) // PUSH4 selector
		a.raw(0x14)             // EQ
		a.pushLabel(arm.label)  // PUSH2 dest
		a.raw(0x57)             // JUMPI
	}
	a.raw(0x00) // unknown selector -> STOP

	// arm_transfer: mem[0:32] = value; LOG3(data=mem[0:32], topics=[sig,from,to]).
	a.jumpLabel("arm_transfer")
	emitLadderTransferLog(a, valueWord, transferTopic, fromTopic, toTopic)
	a.raw(0x00) // STOP

	// arm_multi: same Transfer log + LOG0 over the same 32-byte word.
	a.jumpLabel("arm_multi")
	emitLadderTransferLog(a, valueWord, transferTopic, fromTopic, toTopic)
	a.raw(0x60, 0x20, 0x60, 0x00, 0xa0) // PUSH1 32; PUSH1 0; LOG0
	a.raw(0x00)                         // STOP

	// arm_zero: LOG0 with 32 (zero) bytes, no topics.
	a.jumpLabel("arm_zero")
	a.raw(0x60, 0x20, 0x60, 0x00, 0xa0) // PUSH1 32; PUSH1 0; LOG0
	a.raw(0x00)                         // STOP

	// arm_long: CODECOPY the 256-byte blob to mem[0:256]; LOG1(topic, 256B).
	a.jumpLabel("arm_long")
	a.pushUint(uint64(len(longData))) // length
	a.pushLabel("long_data")          // code offset
	a.raw(0x60, 0x00)                 // dest 0
	a.raw(0x39)                       // CODECOPY
	a.pushHash(longTopic)             // topic0
	a.pushUint(uint64(len(longData))) // size
	a.raw(0x60, 0x00)                 // offset
	a.raw(0xa1)                       // LOG1
	a.raw(0x00)                       // STOP

	a.label("long_data")
	a.raw(longData...)
	return a.assemble()
}

// emitLadderTransferLog stores valueWord at mem[0:32] and emits
// LOG3(mem[0:32], [sig, from, to]).
func emitLadderTransferLog(a *ladderAsm, valueWord, sig, from, to common.Hash) {
	a.pushHash(valueWord)
	a.raw(0x60, 0x00, 0x52) // PUSH1 0; MSTORE
	// LOGn pops (mStart, mSize, topic0, topic1, ...): topics must be pushed in
	// reverse order and mStart last.
	a.pushHash(to)                      // topics[2]
	a.pushHash(from)                    // topics[1]
	a.pushHash(sig)                     // topics[0]
	a.raw(0x60, 0x20, 0x60, 0x00, 0xa3) // PUSH1 32; PUSH1 0; LOG3
}

// ---------------------------------------------------------------------
// In-chain CREATE deploy of the extended logs contract.
// ---------------------------------------------------------------------

// ladderContractDeployInitCode returns init code that returns runtime verbatim
// (PUSH2 len; PUSH2 offset; PUSH1 0; CODECOPY; PUSH2 len; PUSH1 0; RETURN). The
// size/offset are PUSH2 because the runtime exceeds 255 bytes.
func ladderContractDeployInitCode(runtime []byte) []byte {
	if len(runtime) == 0 || len(runtime) > 0xffff {
		panic("ladderContractDeployInitCode: bad runtime size")
	}
	a := newLadderAsm()
	a.pushUint(uint64(len(runtime))) // length
	a.pushLabel("runtime")           // code offset
	a.raw(0x60, 0x00)                // dest 0
	a.raw(0x39)                      // CODECOPY
	a.pushUint(uint64(len(runtime))) // size
	a.raw(0x60, 0x00)                // offset 0
	a.raw(0xf3)                      // RETURN
	a.label("runtime")
	a.raw(runtime...)
	return a.assemble()
}

// ---------------------------------------------------------------------
// Storage-churn contract.
// ---------------------------------------------------------------------

// ladderStorageChurnCode reads base = CALLDATALOAD(0) and prev = CALLDATALOAD(32)
// (both 32-byte slot numbers) and:
//
//   - for j in 0..7: SSTORE(prev+j, 0)          -- cold clear of the previous
//     probe's group (SSTORE reset 5000 + cold 2100; refund when non-zero);
//   - for j in 0..7: SSTORE(base+j, 0x11+j) then SSTORE(base+j, 0x31+j)
//     -- cold write followed by a warm rewrite of the same slot (EIP-2929).
//
// K=8 is fixed by the unrolled loop. Only the base group survives in the
// post-state; the caller declares exactly those K slots.
func ladderStorageChurnCode() []byte {
	a := newLadderAsm()
	for j := 0; j < ladderChurnSlotCount; j++ {
		a.pushUint(0)           // value 0
		a.raw(0x60, 0x20, 0x35) // PUSH1 32; CALLDATALOAD -> prev
		a.pushUint(uint64(j))   // PUSH1 j
		a.raw(0x01, 0x55)       // ADD; SSTORE
	}
	for j := 0; j < ladderChurnSlotCount; j++ {
		a.pushUint(0x11 + uint64(j))
		a.raw(0x60, 0x00, 0x35) // PUSH1 0; CALLDATALOAD -> base
		a.pushUint(uint64(j))
		a.raw(0x01, 0x55) // ADD; SSTORE (cold write)
		a.pushUint(0x31 + uint64(j))
		a.raw(0x60, 0x00, 0x35)
		a.pushUint(uint64(j))
		a.raw(0x01, 0x55) // ADD; SSTORE (warm rewrite)
	}
	a.raw(0x00) // STOP
	return a.assemble()
}

// ladderChurnCalldata encodes (base, prev) for probe call sequence number seq
// (0-based, incremented ONLY on blocks that actually carry the probes). That
// sequence is contiguous, so every call after the first clears the slot group
// the previous probe left non-zero -- including across the %100==0 and fork
// activation blocks where probes are skipped. (A block-index-derived group
// would leave a stale group behind there and make the clear hit an all-zero
// group.) seq==0 uses prev == base so the first call is a zero-then-write of
// its own group.
func ladderChurnCalldata(probeSeq int) hexutil.Bytes {
	if probeSeq < 0 {
		panic("ladderChurnCalldata: negative probe sequence")
	}
	base := uint64(probeSeq) * ladderChurnSlotStride
	prev := base
	if probeSeq > 0 {
		prev = uint64(probeSeq-1) * ladderChurnSlotStride
	}
	out := make([]byte, 64)
	binary.BigEndian.PutUint64(out[24:32], base)
	binary.BigEndian.PutUint64(out[56:64], prev)
	return out
}

// ladderChurnSlots returns the K slots probe sequence number seq leaves
// non-zero (the base group). emitPostState hard-fails on an undeclared written
// slot, so generateLadderChain declares these in ExtraStorage for the block.
func ladderChurnSlots(probeSeq int) []common.Hash {
	if probeSeq < 0 {
		panic("ladderChurnSlots: negative probe sequence")
	}
	base := uint64(probeSeq) * ladderChurnSlotStride
	out := make([]common.Hash, ladderChurnSlotCount)
	for j := 0; j < ladderChurnSlotCount; j++ {
		out[j] = common.BigToHash(new(big.Int).SetUint64(base + uint64(j)))
	}
	return out
}

// ---------------------------------------------------------------------
// Revert-with-reason and INVALID-opcode contracts.
// ---------------------------------------------------------------------

var ladderRevertReasonMsg = []byte("opt8n-ref ladder revert")

// ladderRevertReasonPayload is abi.encodeWithSignature("Error(string)", msg):
// selector + offset(0x20) + length + msg padded to 32.
func ladderRevertReasonPayload() []byte {
	out := make([]byte, 0, 4+32*3)
	out = append(out, crypto.Keccak256([]byte("Error(string)"))[:4]...)
	word := make([]byte, 32)
	word[31] = 0x20
	out = append(out, word...)
	binary.BigEndian.PutUint64(word[24:32], uint64(len(ladderRevertReasonMsg)))
	out = append(out, word...)
	msgWord := make([]byte, 32)
	copy(msgWord, ladderRevertReasonMsg)
	return append(out, msgWord...)
}

// ladderRevertReasonCode copies a fixed abi-encoded Error(string) payload from
// code to memory and REVERTs with it: receipt status 0 + non-empty output.
func ladderRevertReasonCode() []byte {
	payload := ladderRevertReasonPayload()
	a := newLadderAsm()
	a.pushUint(uint64(len(payload))) // length
	a.pushLabel("payload")           // code offset
	a.raw(0x60, 0x00)                // dest 0
	a.raw(0x39)                      // CODECOPY
	a.pushUint(uint64(len(payload))) // size
	a.raw(0x60, 0x00)                // offset 0
	a.raw(0xfd)                      // REVERT
	a.label("payload")
	a.raw(payload...)
	return a.assemble()
}

// ladderInvalidOpcodeCode is a lone INVALID (0xfe): status 0, empty output,
// gasUsed == gasLimit (all gas consumed), distinct from REVERT's partial gas.
func ladderInvalidOpcodeCode() []byte { return []byte{0xfe} }

// ---------------------------------------------------------------------
// Injection rules.
// ---------------------------------------------------------------------

// ladderContractProbesEnabled reports whether block index blockIdx carries the
// contract-layer probes. Every 25 blocks, but skipped on:
//   - the existing %100==0 CREATE probe block (keeps that block's tx list and
//     gas shape unchanged), and
//   - ANY fork activation block, so the fork-boundary blocks stay exactly as
//     before (and the jovian activation block stays deposits-only).
func ladderContractProbesEnabled(blockIdx int, isAnyForkActivation bool) bool {
	if blockIdx <= 0 || blockIdx%ladderContractProbeInterval != 0 {
		return false
	}
	if blockIdx%100 == 0 {
		return false
	}
	return !isAnyForkActivation
}

// injectLadderContractProbes appends the probe txs after every existing recipe
// tx. nextNonce returns bg.TxNonce(senderAddr) at injection time so the key-1
// nonces chain deterministically; add is the caller's four-way-sync closure.
// probeSeq is the count of prior injected probe blocks (see ladderChurnSlots).
func injectLadderContractProbes(add func(inputTx), nextNonce func() uint64, in *inputCase, probeSeq int) {
	// 1) genesis logs predeploy: all four event shapes.
	for i := range ladderLogsProbeSelectors {
		add(transferTx(1, nextNonce(), ladderLogsProbeAddr, big.NewInt(0),
			ladderLogsProbeGas, ladderLogsProbeSelectors[i]))
	}

	// 2) in-chain CREATE of the same contract + one call on it (the call's `to`
	// re-derives from the create nonce; the created account lands in postState
	// via receipts[i].ContractAddress).
	deployNonce := nextNonce()
	add(createTx(1, deployNonce, ladderLogsDeployGas,
		ladderContractDeployInitCode(ladderLogsProbeCode())))
	created := crypto.CreateAddress(addrOfKey(1), deployNonce)
	add(transferTx(1, nextNonce(), created, big.NewInt(0),
		ladderLogsProbeGas, ladderLogsProbeSelectors[0]))

	// 3) storage churn: declare the K slots this call leaves non-zero.
	add(transferTx(1, nextNonce(), ladderStorageProbeAddr, big.NewInt(0),
		ladderChurnGas, ladderChurnCalldata(probeSeq)))
	if in.ExtraStorage == nil {
		in.ExtraStorage = map[common.Address][]common.Hash{}
	}
	in.ExtraStorage[ladderStorageProbeAddr] = append(
		in.ExtraStorage[ladderStorageProbeAddr], ladderChurnSlots(probeSeq)...)

	// 4) revert with reason data (status 0, non-empty output).
	add(transferTx(1, nextNonce(), ladderRevertProbeAddr, big.NewInt(0),
		ladderRevertProbeGas, nil))

	// 5) INVALID opcode (status 0, empty output, gasUsed == limit).
	add(transferTx(1, nextNonce(), ladderInvalidProbeAddr, big.NewInt(0),
		ladderInvalidProbeGas, nil))
}
