package marketdata

import (
	"fmt"
	"strings"
)

// binanceQuoteAssets are the quote assets recognized when splitting a
// concatenated Binance symbol back into canonical BASE/QUOTE form, matched
// in order (longest first) so BTCUSDT resolves as BTC/USDT, never BTCUSD+T.
var binanceQuoteAssets = []string{
	"FDUSD", "USDT", "USDC", "TUSD", "BUSD", "BNB", "BTC", "ETH", "DAI", "EUR", "TRY", "USD",
}

// ToBinance maps a canonical BASE/QUOTE symbol to the Binance concatenated
// uppercase form: "BTC/USDT" -> "BTCUSDT". WS stream names lowercase this
// separately; venue forms never leak out of this package.
func ToBinance(canonical string) (string, error) {
	base, quote, ok := strings.Cut(canonical, "/")
	if !ok || base == "" || quote == "" || strings.Contains(quote, "/") {
		return "", fmt.Errorf("marketdata: invalid canonical symbol %q (want BASE/QUOTE)", canonical)
	}
	return strings.ToUpper(base) + strings.ToUpper(quote), nil
}

// FromBinance maps a Binance concatenated symbol back to canonical
// BASE/QUOTE: "BTCUSDT" -> "BTC/USDT". The symbol must end in one of the
// recognized quote assets and have a non-empty base.
func FromBinance(venue string) (string, error) {
	v := strings.ToUpper(venue)
	for _, q := range binanceQuoteAssets {
		if len(v) > len(q) && strings.HasSuffix(v, q) {
			return v[:len(v)-len(q)] + "/" + q, nil
		}
	}
	return "", fmt.Errorf("marketdata: cannot split venue symbol %q into BASE/QUOTE", venue)
}
