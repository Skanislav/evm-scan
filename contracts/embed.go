// Package contracts embeds the compiled contract artifacts.
//
// Committing the build output keeps `go build` free of a Node/solc dependency; the
// Makefile's `contracts` target regenerates it, and CI can diff the result to prove
// the committed artifact still matches the source.
package contracts

import (
	"embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

//go:embed out/*.json
var artifactFS embed.FS

// Artifact is the subset of a solc artifact we consume.
type Artifact struct {
	ContractName     string          `json:"contractName"`
	SourceName       string          `json:"sourceName"`
	ABI              json.RawMessage `json:"abi"`
	Bytecode         string          `json:"bytecode"`
	DeployedBytecode string          `json:"deployedBytecode"`
}

// Parsed returns the artifact's ABI.
func (a Artifact) Parsed() (abi.ABI, error) {
	parsed, err := abi.JSON(strings.NewReader(string(a.ABI)))
	if err != nil {
		return abi.ABI{}, fmt.Errorf("contracts: parse %s ABI: %w", a.ContractName, err)
	}
	return parsed, nil
}

// Creation returns the deployable creation bytecode.
func (a Artifact) Creation() []byte { return common.FromHex(a.Bytecode) }

// Load returns a compiled contract by name, e.g. "HintRegistry".
func Load(name string) (Artifact, error) {
	body, err := artifactFS.ReadFile("out/" + name + ".json")
	if err != nil {
		return Artifact{}, fmt.Errorf("contracts: %s not compiled (run `make contracts`): %w", name, err)
	}
	var a Artifact
	if err := json.Unmarshal(body, &a); err != nil {
		return Artifact{}, fmt.Errorf("contracts: decode %s artifact: %w", name, err)
	}
	return a, nil
}

// HintRegistryArtifact returns the compiled HintRegistry.
func HintRegistryArtifact() (Artifact, error) { return Load("HintRegistry") }

// HintRegistryABI returns the parsed HintRegistry ABI.
func HintRegistryABI() (abi.ABI, error) {
	a, err := HintRegistryArtifact()
	if err != nil {
		return abi.ABI{}, err
	}
	return a.Parsed()
}
