package api

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Skanislav/evm-scan/internal/ccip"
	"github.com/Skanislav/evm-scan/internal/ens"
)

// keySigner is the Signer a test holds the key of.
type keySigner struct{ key *ecdsa.PrivateKey }

func (k keySigner) Sender() common.Address { return crypto.PubkeyToAddress(k.key.PublicKey) }
func (k keySigner) Sign(digest [32]byte) ([]byte, error) {
	sig, err := crypto.Sign(digest[:], k.key)
	if err != nil {
		return nil, err
	}
	sig[64] += 27
	return sig, nil
}

func signedGatewayServer(t *testing.T, signer keySigner, resolver common.Address) *Server {
	t.Helper()
	return New(Deps{Chains: chainsWith(1), Signer: signer, ENSResolver: resolver, Log: slog.Default()})
}

func resolveRequest(t *testing.T, name, key string) []byte {
	t.Helper()
	resABI, err := loadSignedResolverABI()
	if err != nil {
		t.Fatal(err)
	}
	profile, err := ens.TextCallData(ens.Namehash(name), key)
	if err != nil {
		t.Fatal(err)
	}
	req, err := resABI.Pack("resolve", ens.DNSEncode(name), profile)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// The chain record needs no store and no registry, so it is the one that proves
// the signing path on its own: the answer decodes, and recovers to the signer over
// exactly the bytes the resolver will hash.
func TestENSGatewaySignsChainRecord(t *testing.T) {
	key, _ := crypto.GenerateKey()
	signer := keySigner{key}
	resolver := common.HexToAddress("0x1111111111111111111111111111111111111111")
	s := signedGatewayServer(t, signer, resolver)

	account := common.HexToAddress("0xAbCdEf0123456789abcdef0123456789ABCDEF01")
	name := ens.HintName(account, "evm-scan.eth", 1, false)
	req := resolveRequest(t, name, "evmscan.chain")

	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ens/"+resolver.Hex()+"/"+hexutil.Encode(req)+".json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var body struct{ Data string }
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	result, expires, sig, err := ccip.DecodeSignedResponse(hexutil.MustDecode(body.Data))
	if err != nil {
		t.Fatal(err)
	}
	if text, err := ens.DecodeString(result); err != nil || text != "1" {
		t.Fatalf("evmscan.chain = %q, %v", text, err)
	}
	h := ccip.SignedResponseHash(resolver, expires, req, result)
	raw := append([]byte{}, sig...)
	raw[64] -= 27
	pub, err := crypto.SigToPub(h[:], raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := crypto.PubkeyToAddress(*pub); got != signer.Sender() {
		t.Fatalf("signed by %s, want %s", got.Hex(), signer.Sender().Hex())
	}

	// The POST form carries the same request and answers the same way.
	body2, _ := json.Marshal(map[string]string{"sender": resolver.Hex(), "data": hexutil.Encode(req)})
	post := httptest.NewRequest(http.MethodPost, "/ens", bytes.NewReader(body2))
	post.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	s.mux.ServeHTTP(rec, post)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestENSGatewayRefusesOtherSender(t *testing.T) {
	key, _ := crypto.GenerateKey()
	resolver := common.HexToAddress("0x1111111111111111111111111111111111111111")
	s := signedGatewayServer(t, keySigner{key}, resolver)
	req := resolveRequest(t, ens.HintName(common.Address{1}, "evm-scan.eth", 1, false), "evmscan.chain")
	other := common.HexToAddress("0x2222222222222222222222222222222222222222")

	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ens/"+other.Hex()+"/"+hexutil.Encode(req), nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d for a foreign sender, want 400", rec.Code)
	}
}

func TestENSGatewayWithoutSignerIs503(t *testing.T) {
	s := New(Deps{Chains: chainsWith(1), Log: slog.Default()})
	req := resolveRequest(t, ens.HintName(common.Address{1}, "evm-scan.eth", 1, false), "evmscan.chain")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ens/0x1111111111111111111111111111111111111111/"+hexutil.Encode(req), nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d without a signer, want 503", rec.Code)
	}
}
