package config

import "testing"

// The RPC bill is request count times a rate, so the arithmetic that turns one
// into the other should not be guessed at in a template somewhere.
func TestCostRate(t *testing.T) {
	c := Cost{CUPerRequest: 20, USDPerMillionCU: 0.26}
	// 20 CU at $0.26/M = $0.0000052 a call, so a million calls is $5.20.
	if got := c.Rate() * 1e6; got < 5.19 || got > 5.21 {
		t.Errorf("a million requests = $%.4f, want ~$5.20", got)
	}
	// An unpriced provider reports no money rather than zero-dollar traffic.
	for _, unpriced := range []Cost{{}, {CUPerRequest: 20}, {USDPerMillionCU: 0.26}} {
		if unpriced.Rate() != 0 {
			t.Errorf("%+v: want an unpriced rate of 0, got %v", unpriced, unpriced.Rate())
		}
	}
}
