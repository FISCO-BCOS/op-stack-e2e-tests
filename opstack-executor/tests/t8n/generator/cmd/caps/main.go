// Command caps emits the Engine API capability list that op-geth advertises by
// reflection, so the matrix can assert FISCO's own advertisement derives from the
// same naming rule. Reflection on the type is used because constructing
// ConsensusAPI requires a full eth backend.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"unicode"

	"github.com/ethereum/go-ethereum/eth/catalyst"
)

type artifact struct {
	Pin         string   `json:"pin"`
	GeneratedBy string   `json:"generated_by"`
	Caps        []string `json:"caps"`
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: caps <op-geth-pin> <out-dir>")
		os.Exit(2)
	}
	pin, outDir := os.Args[1], os.Args[2]
	// Same rule as op-geth's ConsensusAPI.ExchangeCapabilities
	// (eth/catalyst/api.go: pin d0734fd5 -> :1122-1133; the corpus pin e8800cffe -> :1123-1134;
	// the two bodies are byte-identical): "engine_" + lower-camel of every exported method
	// except the RPC entry point itself.
	t := reflect.TypeOf((*catalyst.ConsensusAPI)(nil))
	caps := make([]string, 0, t.NumMethod())
	for i := 0; i < t.NumMethod(); i++ {
		name := []rune(t.Method(i).Name)
		if string(name) == "ExchangeCapabilities" {
			continue
		}
		caps = append(caps, "engine_"+string(unicode.ToLower(name[0]))+string(name[1:]))
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	f, err := os.Create(filepath.Join(outDir, "caps.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(artifact{Pin: pin, GeneratedBy: "cmd/caps", Caps: caps}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
