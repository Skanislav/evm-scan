package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/Skanislav/evm-scan/contracts"
	"github.com/Skanislav/evm-scan/internal/ccip"
	"github.com/Skanislav/evm-scan/internal/ens"
	"github.com/Skanislav/evm-scan/internal/store"
)

// The signed ENS gateway.
//
// HintResolver answers `<hex>.hints.<parent>` by handing a gateway's answer to the
// registry, which checks it against the committed root. That only works on the
// registry's own chain. ENS names live on mainnet and the registry lives on Base, so
// the name there is served by HintSignedResolver, which checks a signature instead:
// this handler produces the same records and signs each answer with the publisher's
// key, and the resolver's callback recovers the signer and checks an expiry.
//
// Be exact about what that is. A signature says the publisher answered this
// request this way at this time; it does not say the answer is in a root. The
// records themselves are built from the latest finalized epoch's committed leaf —
// the same path /ccip proves — so an honest publisher signs what the root commits,
// and a dishonest one can be caught by comparing against the registry on Base. What
// a signature cannot do is stop a stolen publisher key from forging a record, which
// a proof would.
//
// The request that is signed is the resolver's full `resolve(name, data)` calldata,
// exactly as received. It is decoded here to know what to answer and hashed as
// bytes to sign; nothing is re-encoded, so gateway and contract cannot disagree
// about what was asked.

const ensAnswerTTL = 5 * time.Minute

var (
	signedResolverOnce sync.Once
	signedResolverABI  abi.ABI
	signedResolverErr  error
)

func loadSignedResolverABI() (abi.ABI, error) {
	signedResolverOnce.Do(func() {
		art, err := contracts.Load("HintSignedResolver")
		if err != nil {
			signedResolverErr = err
			return
		}
		signedResolverABI, signedResolverErr = art.Parsed()
	})
	return signedResolverABI, signedResolverErr
}

func (s *Server) ensGet(w http.ResponseWriter, r *http.Request) {
	s.serveENS(w, r, r.PathValue("sender"), strings.TrimSuffix(r.PathValue("data"), ".json"))
}

func (s *Server) ensPost(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Sender string `json:"sender"`
		Data   string `json:"data"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad body", err)
		return
	}
	s.serveENS(w, r, body.Sender, body.Data)
}

func (s *Server) serveENS(w http.ResponseWriter, r *http.Request, senderHex, dataHex string) {
	if s.d.Signer == nil {
		writeErr(w, http.StatusServiceUnavailable, "this deployment holds no publisher key, so it signs nothing", nil)
		return
	}
	sender, err := parseAddress(senderHex)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad sender", err)
		return
	}
	// The signature binds the sender, so an answer produced for the wrong one is
	// useless to an honest client and a gift to anyone impersonating the resolver.
	if s.d.ENSResolver != (common.Address{}) && sender != s.d.ENSResolver {
		writeErr(w, http.StatusBadRequest, "not this deployment's resolver", nil)
		return
	}
	request, err := hexutil.Decode(dataHex)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad calldata", err)
		return
	}

	resABI, err := loadSignedResolverABI()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "resolver ABI unavailable", err)
		return
	}
	resolve := resABI.Methods["resolve"]
	if len(request) < 4 || !bytes.Equal(request[:4], resolve.ID) {
		writeErr(w, http.StatusBadRequest, "unsupported selector "+ccip.Selector(request), nil)
		return
	}
	vals, err := resolve.Inputs.Unpack(request[4:])
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad resolve arguments", err)
		return
	}
	wireName, _ := vals[0].([]byte)
	profile, _ := vals[1].([]byte)

	name, err := ens.DNSDecode(wireName)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad name", err)
		return
	}
	account, chainID, hasChain, ok := ens.ParseHintName(name)
	if !ok {
		writeErr(w, http.StatusBadRequest, "the first label is not an account", nil)
		return
	}
	if !hasChain {
		// The same rule hintName() applies when it prints a name: the first
		// configured chain is the default one, and the resolver's defaultChainId
		// must agree with it.
		first, ok := s.d.Chains.First()
		if !ok {
			writeErr(w, http.StatusServiceUnavailable, "no chain running", nil)
			return
		}
		chainID = first.ID
	}
	if len(profile) < 4 || !bytes.Equal(profile[:4], ens.Selector("text(bytes32,string)")) {
		writeErr(w, http.StatusBadRequest, "only text records are answered here; the resolver serves addr itself", nil)
		return
	}
	key, err := ens.TextKey(profile)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad text arguments", err)
		return
	}

	text, err := s.ensRecord(r, chainID, account, key)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "could not build the record", err)
		return
	}
	result, err := ens.EncodeString(text)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "encode failed", err)
		return
	}
	expires := uint64(time.Now().Add(ensAnswerTTL).Unix())
	sig, err := s.d.Signer.Sign(ccip.SignedResponseHash(sender, expires, request, result))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "sign failed", err)
		return
	}
	resp, err := ccip.EncodeSignedResponse(result, expires, sig)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "encode failed", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"data": hexutil.Encode(resp)})
}

// ensRecord is the text a key resolves to for an account, "" when there is nothing
// to say. Every epoch-derived value comes from the latest finalized epoch, the same
// one /ccip proves, so a signed answer and a proven one agree.
func (s *Server) ensRecord(r *http.Request, chainID uint64, account common.Address, key string) (string, error) {
	ctx := r.Context()
	switch key {
	case "evmscan.chain":
		return fmt.Sprint(chainID), nil
	case "evmscan.registry":
		if s.d.Registry == nil {
			return "", nil
		}
		return fmt.Sprintf("eip155:%d:%s", s.d.RegistryChainID, strings.ToLower(s.d.Registry.Address().Hex())), nil
	case "evmscan.hint":
		raw, _, err := s.accountHintBytes(ctx, account)
		if errors.Is(err, store.ErrNotFound) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		return base64.RawURLEncoding.EncodeToString(raw), nil
	case "evmscan.contracts", "evmscan.epoch", "evmscan.range", "evmscan.root", "evmscan.uri":
		// Fall through to the epoch-derived records below.
	default:
		return "", nil
	}

	if s.d.Registry == nil {
		return "", nil
	}
	e, err := s.latestFinalizedFor(ctx, chainID)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	switch key {
	case "evmscan.epoch":
		if e.OnchainID == nil {
			return "", nil
		}
		return fmt.Sprint(*e.OnchainID), nil
	case "evmscan.range":
		return fmt.Sprintf("%d-%d", e.FromBlock, e.ToBlock), nil
	case "evmscan.root":
		return strings.ToLower(e.MerkleRoot.Hex()), nil
	case "evmscan.uri":
		return e.URI, nil
	}
	pr, err := s.accountProof(ctx, e, account)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return ens.JoinContracts(pr.assets), nil
}
