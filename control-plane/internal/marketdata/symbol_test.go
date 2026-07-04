package marketdata

import "testing"

func TestToBinance(t *testing.T) {
	tests := []struct {
		name      string
		canonical string
		want      string
		wantErr   bool
	}{
		{"btc usdt", "BTC/USDT", "BTCUSDT", false},
		{"eth btc", "ETH/BTC", "ETHBTC", false},
		{"lowercase input", "btc/usdt", "BTCUSDT", false},
		{"stablecoin base", "TUSD/USDT", "TUSDUSDT", false},
		{"missing slash", "BTCUSDT", "", true},
		{"empty base", "/USDT", "", true},
		{"empty quote", "BTC/", "", true},
		{"double slash", "BTC/USD/T", "", true},
		{"empty string", "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ToBinance(tc.canonical)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ToBinance(%q) error = %v, wantErr %v", tc.canonical, err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("ToBinance(%q) = %q, want %q", tc.canonical, got, tc.want)
			}
		})
	}
}

func TestFromBinance(t *testing.T) {
	tests := []struct {
		name    string
		venue   string
		want    string
		wantErr bool
	}{
		{"btc usdt", "BTCUSDT", "BTC/USDT", false},
		{"eth btc", "ETHBTC", "ETH/BTC", false},
		{"lowercase stream form", "btcusdt", "BTC/USDT", false},
		{"longest quote wins", "TUSDUSDT", "TUSD/USDT", false},
		{"usdt base try quote", "USDTTRY", "USDT/TRY", false},
		{"unknown quote", "BTCXYZ", "", true},
		{"bare quote asset", "USDT", "", true},
		{"empty string", "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FromBinance(tc.venue)
			if (err != nil) != tc.wantErr {
				t.Fatalf("FromBinance(%q) error = %v, wantErr %v", tc.venue, err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("FromBinance(%q) = %q, want %q", tc.venue, got, tc.want)
			}
		})
	}
}

func TestSymbolRoundTrip(t *testing.T) {
	for _, canonical := range []string{"BTC/USDT", "ETH/BTC", "SOL/USDC"} {
		venue, err := ToBinance(canonical)
		if err != nil {
			t.Fatalf("ToBinance(%q): %v", canonical, err)
		}
		back, err := FromBinance(venue)
		if err != nil {
			t.Fatalf("FromBinance(%q): %v", venue, err)
		}
		if back != canonical {
			t.Fatalf("round trip %q -> %q -> %q", canonical, venue, back)
		}
	}
}
