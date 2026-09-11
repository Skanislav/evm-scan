package hintreg

// ABI vectors for the TypeScript mirror in ../../mirror.
//
// A mirror has to read the committed root off the chain itself — a root handed to
// it by the same service that served the rows proves nothing. That means encoding
// two calls and decoding their returns, and the returns are not simple: the Epoch
// struct is a dynamic tuple, so a decoder that gets one offset wrong reads a
// plausible-looking wrong root.
//
// So the blobs here are packed by go-ethereum from the real compiled ABI in
// contracts/out, not written by hand. Regenerate with:
//
//	go test ./internal/hintreg/ -run MirrorFixtures -update-fixtures

import (
	"bytes"
	"encoding/json"
	"flag"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/Skanislav/evm-scan/contracts"
)

var updateMirrorFixtures = flag.Bool("update-fixtures", false,
	"rewrite mirror/testdata/registry.json from the compiled ABI")

const mirrorFixturePath = "../../mirror/testdata/registry.json"

// epochTuple mirrors HintRegistry.Epoch for abi packing. go-ethereum matches tuple
// components to fields by capitalising the component name, so the field names here
// are not free.
type epochTuple struct {
	ChainId           uint64
	FromBlock         uint64
	ToBlock           uint64
	Root              [32]byte
	CoverageRoot      [32]byte
	Uri               string
	Publisher         common.Address
	Challenger        common.Address
	Bond              *big.Int
	PublishedAt       uint64
	ChallengeDeadline uint64
	Status            uint8
	AssertionId       [32]byte
}

type registryFixture struct {
	Note     string `json:"note"`
	Registry string `json:"registry"`

	LatestFinalizedEpoch struct {
		// Calldata for chainId, so the TypeScript encoder is checked too.
		ChainID  string `json:"chain_id"`
		Calldata string `json:"calldata"`
		// Returndata, and what it decodes to.
		Returndata string         `json:"returndata"`
		Found      bool           `json:"found"`
		EpochID    string         `json:"epoch_id"`
		Epoch      decodedEpoch   `json:"epoch"`
		NotFound   notFoundVector `json:"not_found"`
	} `json:"latest_finalized_epoch"`

	GetEpoch struct {
		EpochID    string       `json:"epoch_id"`
		Calldata   string       `json:"calldata"`
		Returndata string       `json:"returndata"`
		Epoch      decodedEpoch `json:"epoch"`
	} `json:"get_epoch"`
}

// notFoundVector is the answer for a chain with no finalized epoch. A decoder that
// reads the struct without checking `found` first would take a zero root as a real
// commitment, and every empty mirror would then verify against it.
type notFoundVector struct {
	Why        string `json:"why"`
	Returndata string `json:"returndata"`
	Found      bool   `json:"found"`
}

type decodedEpoch struct {
	ChainID           string `json:"chain_id"`
	FromBlock         string `json:"from_block"`
	ToBlock           string `json:"to_block"`
	Root              string `json:"root"`
	CoverageRoot      string `json:"coverage_root"`
	URI               string `json:"uri"`
	Publisher         string `json:"publisher"`
	Challenger        string `json:"challenger"`
	Bond              string `json:"bond"`
	PublishedAt       string `json:"published_at"`
	ChallengeDeadline string `json:"challenge_deadline"`
	Status            uint8  `json:"status"`
	AssertionID       string `json:"assertion_id"`
}

func view(e epochTuple) decodedEpoch {
	return decodedEpoch{
		ChainID:           strconv.FormatUint(e.ChainId, 10),
		FromBlock:         strconv.FormatUint(e.FromBlock, 10),
		ToBlock:           strconv.FormatUint(e.ToBlock, 10),
		Root:              common.Hash(e.Root).Hex(),
		CoverageRoot:      common.Hash(e.CoverageRoot).Hex(),
		URI:               e.Uri,
		Publisher:         e.Publisher.Hex(),
		Challenger:        e.Challenger.Hex(),
		Bond:              e.Bond.String(),
		PublishedAt:       strconv.FormatUint(e.PublishedAt, 10),
		ChallengeDeadline: strconv.FormatUint(e.ChallengeDeadline, 10),
		Status:            e.Status,
		AssertionID:       common.Hash(e.AssertionId).Hex(),
	}
}

func hash(b byte) [32]byte {
	var h [32]byte
	for i := range h {
		h[i] = b
	}
	return h
}

func TestMirrorFixtures(t *testing.T) {
	registryABI, err := contracts.HintRegistryABI()
	if err != nil {
		t.Fatalf("load abi: %v", err)
	}

	// A commitment with a non-empty uri, because the uri is what makes the tuple
	// dynamic: with it empty, a decoder that mishandles the offset still works.
	epoch := epochTuple{
		ChainId:      8453,
		FromBlock:    25935372,
		ToBlock:      25944437,
		Root:         hash(0xab),
		CoverageRoot: hash(0xcd),
		Uri:          "https://evmscan.example/v1/epochs/2/snapshot",
		Publisher:    common.HexToAddress("0x00000000000000000000000000000000000000e0"),
		// A challenged-then-resolved epoch still has a challenger recorded, so the
		// field is not left zero here: a decoder must not skip it.
		Challenger:        common.HexToAddress("0x00000000000000000000000000000000000000e1"),
		Bond:              big.NewInt(1_000_000_000_000_000_000),
		PublishedAt:       1757000000,
		ChallengeDeadline: 1757003600,
		Status:            3, // EpochFinalized; None=0, Proposed, Challenged, Finalized, Rejected
		AssertionId:       hash(0xef),
	}

	latest := registryABI.Methods["latestFinalizedEpoch"]
	get := registryABI.Methods["getEpoch"]

	latestReturn, err := latest.Outputs.Pack(true, big.NewInt(2), epoch)
	if err != nil {
		t.Fatalf("pack latestFinalizedEpoch: %v", err)
	}
	notFoundReturn, err := latest.Outputs.Pack(false, big.NewInt(0), epochTuple{Bond: big.NewInt(0)})
	if err != nil {
		t.Fatalf("pack latestFinalizedEpoch (empty): %v", err)
	}
	getReturn, err := get.Outputs.Pack(epoch)
	if err != nil {
		t.Fatalf("pack getEpoch: %v", err)
	}
	latestCalldata, err := registryABI.Pack("latestFinalizedEpoch", uint64(8453))
	if err != nil {
		t.Fatalf("pack latestFinalizedEpoch calldata: %v", err)
	}
	getCalldata, err := registryABI.Pack("getEpoch", big.NewInt(2))
	if err != nil {
		t.Fatalf("pack getEpoch calldata: %v", err)
	}

	var f registryFixture
	f.Note = "generated by internal/hintreg/mirror_fixtures_test.go; regenerate with " +
		"go test ./internal/hintreg/ -run MirrorFixtures -update-fixtures"
	f.Registry = common.HexToAddress("0x00000000000000000000000000000000000000ee").Hex()

	f.LatestFinalizedEpoch.ChainID = "8453"
	f.LatestFinalizedEpoch.Calldata = hexutil.Encode(latestCalldata)
	f.LatestFinalizedEpoch.Returndata = hexutil.Encode(latestReturn)
	f.LatestFinalizedEpoch.Found = true
	f.LatestFinalizedEpoch.EpochID = "2"
	f.LatestFinalizedEpoch.Epoch = view(epoch)
	f.LatestFinalizedEpoch.NotFound = notFoundVector{
		Why: "no finalized epoch for this chain: found is false and the struct is " +
			"zeroed, so a decoder that reads the root without checking found would " +
			"accept the zero root as a commitment",
		Returndata: hexutil.Encode(notFoundReturn),
		Found:      false,
	}

	f.GetEpoch.EpochID = "2"
	f.GetEpoch.Calldata = hexutil.Encode(getCalldata)
	f.GetEpoch.Returndata = hexutil.Encode(getReturn)
	f.GetEpoch.Epoch = view(epoch)

	body, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body = append(body, '\n')

	if *updateMirrorFixtures {
		if err := os.MkdirAll(filepath.Dir(mirrorFixturePath), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(mirrorFixturePath, body, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		t.Logf("wrote %s", mirrorFixturePath)
		return
	}

	const how = "regenerate with: go test ./internal/hintreg/ -run MirrorFixtures -update-fixtures"
	got, err := os.ReadFile(mirrorFixturePath)
	if err != nil {
		t.Fatalf("%s: %v\n%s", mirrorFixturePath, err, how)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("%s is stale: the TypeScript decoder is being tested against an ABI "+
			"encoding the contract no longer has.\n%s", mirrorFixturePath, how)
	}
}
