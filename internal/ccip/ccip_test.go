package ccip

import (
	"encoding/hex"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/contracts"
)

func TestParseOffchainLookupNeedsNoABI(t *testing.T) {
	if hex.EncodeToString(OffchainLookupSelector[:]) != "556f1830" {
		t.Fatalf("OffchainLookup selector derived as %x", OffchainLookupSelector)
	}
	sender := common.HexToAddress("0x1234")
	body, err := offchainLookupArgs.Pack(sender, []string{"https://gw/{sender}/{data}.json"}, []byte{1, 2}, [4]byte{9, 9, 9, 9}, []byte{3})
	if err != nil {
		t.Fatal(err)
	}
	data := append(OffchainLookupSelector[:], body...)

	l, ok, err := ParseOffchainLookup(data)
	if err != nil || !ok {
		t.Fatalf("parse: %v %v", ok, err)
	}
	if l.Sender != sender || len(l.URLs) != 1 || string(l.CallData) != "\x01\x02" || l.Callback != [4]byte{9, 9, 9, 9} || string(l.ExtraData) != "\x03" {
		t.Fatalf("lookup %+v", l)
	}
	if _, ok, err := ParseOffchainLookup([]byte{0xde, 0xad, 0xbe, 0xef}); ok || err != nil {
		t.Fatalf("another selector is not a lookup: %v %v", ok, err)
	}

	// The ABI-checked form agrees with the ABI-free one for the registry.
	regABI, err := contracts.HintRegistryABI()
	if err != nil {
		t.Fatal(err)
	}
	l2, ok, err := ParseLookup(regABI, data)
	if err != nil || !ok || l2.Sender != l.Sender {
		t.Fatalf("ParseLookup: %v %v %+v", ok, err, l2)
	}
	if id := regABI.Errors[ErrorName].ID; [4]byte(id[:4]) != OffchainLookupSelector {
		t.Fatal("the registry's OffchainLookup must be the ERC-3668 one")
	}
}
