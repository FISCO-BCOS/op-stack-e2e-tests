// Command opnodewin sweeps the 9 modeled forks over a synthetic schedule and asks the
// pinned op-node which Engine API method versions it selects at each activation
// timestamp. op-node is the sole authority for CL method selection (specs and op-geth
// do not describe it): rollup.Config.{ForkchoiceUpdatedVersion,NewPayloadVersion,GetPayloadVersion}.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ethereum-optimism/optimism/op-node/rollup"
	"github.com/ethereum-optimism/optimism/op-service/eth"
)

type forkIn struct {
	Name string `json:"name"`
	Time uint64 `json:"time"`
}
type scheduleIn struct {
	Forks []forkIn `json:"forks"`
}
type row struct {
	Fork              string `json:"fork"`
	Timestamp         uint64 `json:"timestamp"`
	NewPayload        string `json:"newPayload"`
	ForkchoiceUpdated string `json:"forkchoiceUpdated"`
	GetPayload        string `json:"getPayload"`
}
type artifact struct {
	Pin         string `json:"pin"`
	GeneratedBy string `json:"generated_by"`
	Schedule    string `json:"schedule"`
	Rows        []row  `json:"windows"`
}

func timePtr(v uint64) *uint64 { return &v }

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: opnodewin <optimism-pin> <input-schedule.json> <out-dir>")
		os.Exit(2)
	}
	pin, inPath, outDir := os.Args[1], os.Args[2], os.Args[3]
	raw, err := os.ReadFile(inPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var in scheduleIn
	if err := json.Unmarshal(raw, &in); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	byName := map[string]uint64{}
	for _, f := range in.Forks {
		byName[f.Name] = f.Time
	}
	cfg := &rollup.Config{
		RegolithTime: timePtr(byName["regolith"]),
		CanyonTime:   timePtr(byName["canyon"]),
		EcotoneTime:  timePtr(byName["ecotone"]),
		FjordTime:    timePtr(byName["fjord"]),
		GraniteTime:  timePtr(byName["granite"]),
		HoloceneTime: timePtr(byName["holocene"]),
		IsthmusTime:  timePtr(byName["isthmus"]),
		JovianTime:   timePtr(byName["jovian"]),
	}
	out := artifact{Pin: pin, GeneratedBy: "cmd/opnodewin", Schedule: "generator/matrix/input_schedule.json"}
	for _, f := range in.Forks {
		// Sweep exactly at the activation timestamp: Is<fork>(ts) is >= based.
		ts := f.Time
		attr := &eth.PayloadAttributes{Timestamp: eth.Uint64Quantity(ts)}
		out.Rows = append(out.Rows, row{
			Fork:              f.Name,
			Timestamp:         ts,
			NewPayload:        string(cfg.NewPayloadVersion(ts)),
			ForkchoiceUpdated: string(cfg.ForkchoiceUpdatedVersion(attr)),
			GetPayload:        string(cfg.GetPayloadVersion(ts)),
		})
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	f, err := os.Create(filepath.Join(outDir, "engine_api_windows.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
