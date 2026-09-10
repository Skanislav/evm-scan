package hintreg

import (
	"context"
	"errors"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// A registration names the chain its index is about, which need not be the chain the
// registry sits on. When those are confused, the token named has no code on the
// chain named, and the asset is unindexable by construction: no code, no events,
// ever. Adopting one anyway made the indexer probe a history horizon it had no use
// for, which binary-searches the chain in archive reads, exhausted the RPC quota,
// and took the daemon down. So the mirror checks for code first.
func TestMirrorSkipsTokensWithNoCodeOnTheNamedChain(t *testing.T) {
	token := common.HexToAddress("0x40D16FC0246aD3160Ccc09B8D0D3A2cD28aE6C2f")

	for _, tc := range []struct {
		name    string
		code    CodeFunc
		want    bool
		wantErr bool
	}{
		{
			name: "a real token on its own chain is indexed",
			code: func(context.Context, uint64, common.Address) ([]byte, error) {
				return []byte{0x60, 0x80, 0x60, 0x40}, nil
			},
			want: true,
		},
		{
			name: "the same token on the wrong chain is refused",
			code: func(context.Context, uint64, common.Address) ([]byte, error) {
				return nil, nil // no code: this is the mis-keyed registration
			},
			want: false,
		},
		{
			name: "an empty slice is no code, not unknown",
			code: func(context.Context, uint64, common.Address) ([]byte, error) {
				return []byte{}, nil
			},
			want: false,
		},
		{
			name:    "an RPC failure is not evidence of absence",
			code:    func(context.Context, uint64, common.Address) ([]byte, error) { return nil, errors.New("429") },
			wantErr: true,
		},
		{
			// Refusing on ignorance would silently drop good hints, so no checker
			// means no opinion.
			name: "without a checker the hint is admitted",
			code: nil,
			want: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &Mirror{code: tc.code}
			got, err := m.hasCode(context.Background(), 8453, token)
			if tc.wantErr {
				if err == nil {
					t.Fatal("want an error, got none")
				}
				return
			}
			if err != nil {
				t.Fatalf("hasCode: %v", err)
			}
			if got != tc.want {
				t.Errorf("hasCode = %v, want %v", got, tc.want)
			}
		})
	}
}
