package store

import (
	"encoding/hex"
	"strings"
)

// AccountPrefixRange turns a hex address prefix into the inclusive 20-byte bounds that
// contain every address starting with it.
//
// A range rather than a LIKE, for three reasons: it stays sargable against
// interactions_account_idx, addresses are stored as raw bytes so there is no text to
// match anyway, and — the one that decides it — this is a pure function, testable
// without a database, which is the only kind of test this package can run.
//
// An odd number of nibbles is handled exactly: the last nibble is held fixed and the
// remaining half-byte spans 0x0..0xf.
//
//	""      -> nil, nil, true    (no filter)
//	"0xab"  -> ab00…00, abff…ff, true
//	"0xabc" -> abc0…00, abcf…ff, true
//	"0xzz"  -> nil, nil, false   (not hex)
func AccountPrefixRange(q string) (lo, hi []byte, ok bool) {
	q = strings.TrimSpace(q)
	q = strings.TrimPrefix(strings.TrimPrefix(q, "0X"), "0x")
	if q == "" {
		return nil, nil, true
	}
	if len(q) > 40 {
		return nil, nil, false
	}
	for i := 0; i < len(q); i++ {
		if _, err := hex.DecodeString(string(q[i]) + "0"); err != nil {
			return nil, nil, false
		}
	}

	// Pad to a whole address in both directions: low nibbles go to 0, high nibbles to f.
	loHex := q + strings.Repeat("0", 40-len(q))
	hiHex := q + strings.Repeat("f", 40-len(q))
	lo, err := hex.DecodeString(loHex)
	if err != nil {
		return nil, nil, false
	}
	hi, err = hex.DecodeString(hiHex)
	if err != nil {
		return nil, nil, false
	}
	return lo, hi, true
}
