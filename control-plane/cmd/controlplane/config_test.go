package main

import (
	"strings"
	"testing"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/exchange"
)

// TestParseLiveOMS pins the startup guards (live-oms-and-reconciler.md
// §Config): paper is the default, any unknown mode/env refuses to start,
// credentials are required iff mode=live, and prod demands the exact ack
// literal — and secret values never appear in error text.
func TestParseLiveOMS(t *testing.T) {
	const key, secret = "test-api-key-value", "test-api-secret-value"
	cases := []struct {
		name    string
		mode    string
		env     string
		key     string
		secret  string
		ack     string
		tuning  string
		wantNil bool
		wantErr bool
		wantEnv exchange.Env
	}{
		{name: "default is paper", wantNil: true},
		{name: "explicit paper", mode: "paper", wantNil: true},
		{name: "paper ignores missing keys", mode: "paper", wantNil: true},
		{name: "unknown mode refused", mode: "LIVE", wantErr: true},
		{name: "live testnet default env", mode: "live", key: key, secret: secret, wantEnv: exchange.EnvTestnet},
		{name: "live explicit testnet", mode: "live", env: "testnet", key: key, secret: secret, wantEnv: exchange.EnvTestnet},
		{name: "unknown env refused", mode: "live", env: "mainnet", key: key, secret: secret, wantErr: true},
		{name: "live requires api key", mode: "live", secret: secret, wantErr: true},
		{name: "live requires api secret", mode: "live", key: key, wantErr: true},
		{name: "prod requires ack", mode: "live", env: "prod", key: key, secret: secret, wantErr: true},
		{name: "prod refuses wrong ack", mode: "live", env: "prod", key: key, secret: secret,
			ack: "i-understand-this-trades-real-funds", wantErr: true},
		{name: "prod with exact ack", mode: "live", env: "prod", key: key, secret: secret,
			ack: prodAckLiteral, wantEnv: exchange.EnvProd},
		{name: "bad tuning refused", mode: "live", key: key, secret: secret,
			tuning: `{"bogus":1}`, wantErr: true},
		{name: "tuning applies", mode: "live", key: key, secret: secret,
			tuning: `{"recv_window_ms":9000}`, wantEnv: exchange.EnvTestnet},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := parseLiveOMS(tc.mode, tc.env, tc.key, tc.secret, tc.ack, tc.tuning)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				for _, s := range []string{key, secret} {
					if strings.Contains(err.Error(), s) {
						t.Fatalf("error text leaks a credential: %q", err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLiveOMS: %v", err)
			}
			if tc.wantNil {
				if c != nil {
					t.Fatalf("config = %+v, want nil (paper)", c)
				}
				return
			}
			if c == nil || c.env != tc.wantEnv || c.apiKey != tc.key || c.apiSecret != tc.secret {
				t.Fatalf("config = %+v, want env %s with credentials", c, tc.wantEnv)
			}
			if tc.tuning != "" && c.tuning.RecvWindowMS != 9000 {
				t.Fatalf("tuning.RecvWindowMS = %d, want 9000", c.tuning.RecvWindowMS)
			}
		})
	}
}

// TestValidateVenuePairing: prod trading refuses testnet market-data
// endpoint overrides; testnet trading pairs with anything.
func TestValidateVenuePairing(t *testing.T) {
	if err := validateVenuePairing(exchange.EnvProd, "", ""); err != nil {
		t.Fatalf("prod with prod market data: %v", err)
	}
	if err := validateVenuePairing(exchange.EnvProd, "https://data-api.binance.vision", ""); err != nil {
		t.Fatalf("prod with a prod mirror: %v", err)
	}
	if err := validateVenuePairing(exchange.EnvProd, "https://testnet.binance.vision", ""); err == nil {
		t.Fatal("prod with testnet REST market data: expected refusal")
	}
	if err := validateVenuePairing(exchange.EnvProd, "", "wss://stream.testnet.binance.vision"); err == nil {
		t.Fatal("prod with testnet WS market data: expected refusal")
	}
	if err := validateVenuePairing(exchange.EnvTestnet, "https://testnet.binance.vision", ""); err != nil {
		t.Fatalf("testnet pairing: %v", err)
	}
}
