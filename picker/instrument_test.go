package picker

import (
	"testing"

	"github.com/BROCKUGANDA/alpacaruns/tools"
)

func catalogue() map[string]tools.Asset {
	return map[string]tools.Asset{
		"AAPL": {Symbol: "AAPL", Name: "Apple Inc. Common Stock", Exchange: "NASDAQ",
			AssetClass: tools.AssetClassEquity, Tradable: true, Fractional: true, Shortable: true},
		"SPY": {Symbol: "SPY", Name: "SPDR S&P 500 ETF Trust", Exchange: "ARCA",
			AssetClass: tools.AssetClassEquity, Tradable: true, Fractional: true, Shortable: true},
		"QQQ": {Symbol: "QQQ", Name: "Invesco QQQ Trust", Exchange: "NASDAQ",
			AssetClass: tools.AssetClassEquity, Tradable: true, Fractional: true},
		"GLD": {Symbol: "GLD", Name: "SPDR Gold Shares ETF", Exchange: "ARCA",
			AssetClass: tools.AssetClassEquity, Tradable: true},
	}
}

// TestBuildClassifiesEveryClass proves one registry can hold equities,
// ETFs, index trackers and crypto at once, each with the data family
// and execution window its class needs.
func TestBuildClassifiesEveryClass(t *testing.T) {
	reg := BuildWithCatalogue(catalogue(), Spec{
		Equities: []string{"AAPL"},
		ETFs:     []string{"SPY"},
		Indices:  []string{"QQQ"},
		Crypto:   []string{"BTC/USD"},
	})

	if len(reg.Instruments()) != 4 {
		t.Fatalf("expected 4 instruments, got %d: %+v", len(reg.Instruments()), reg.Instruments())
	}

	aapl, ok := reg.Lookup("AAPL")
	if !ok || aapl.Class != ClassEquity || aapl.Family != FamilyStocks || aapl.Window != "equity" {
		t.Fatalf("AAPL misclassified: %+v", aapl)
	}
	if !aapl.Fractional || !aapl.Shortable {
		t.Fatalf("AAPL metadata lost: %+v", aapl)
	}

	spy, ok := reg.Lookup("SPY")
	if !ok || spy.Class != ClassETF {
		t.Fatalf("SPY must be an ETF: %+v", spy)
	}
	qqq, ok := reg.Lookup("QQQ")
	if !ok || qqq.Class != ClassIndex {
		t.Fatalf("QQQ must be an index tracker: %+v", qqq)
	}

	btc, ok := reg.Lookup("BTCUSD")
	if !ok {
		t.Fatal("crypto must resolve from either symbol form")
	}
	if btc.Class != ClassCrypto || btc.Family != FamilyCrypto || btc.Window != "crypto" {
		t.Fatalf("BTC misclassified: %+v", btc)
	}
	if btc.OrderSymbol() != "BTCUSD" || btc.DataSymbol() != "BTC/USD" {
		t.Fatalf("crypto symbol forms wrong: order=%q data=%q", btc.OrderSymbol(), btc.DataSymbol())
	}
}

// TestBuildKeepsUnknownSymbols proves a symbol the broker catalogue
// does not know is still part of the universe (reported as not
// tradable) instead of silently vanishing.
func TestBuildKeepsUnknownSymbols(t *testing.T) {
	reg := BuildWithCatalogue(catalogue(), Spec{Equities: []string{"ZZZZ"}})
	inst, ok := reg.Lookup("ZZZZ")
	if !ok {
		t.Fatal("unknown symbol must stay in the universe")
	}
	if inst.Tradable {
		t.Fatal("unknown symbol must not be reported as tradable")
	}
}

// TestRegistrySymbolsByClass proves the universe can be sliced by
// class — the auto loop needs exactly that to route data and windows.
func TestRegistrySymbolsByClass(t *testing.T) {
	reg := BuildWithCatalogue(catalogue(), Spec{
		Equities: []string{"AAPL"},
		ETFs:     []string{"SPY"},
		Crypto:   []string{"BTC/USD"},
	})
	eq := reg.Symbols(ClassEquity, ClassETF)
	if len(eq) != 2 {
		t.Fatalf("expected 2 equity-side symbols, got %v", eq)
	}
	cr := reg.Symbols(ClassCrypto)
	if len(cr) != 1 || cr[0] != "BTC/USD" {
		t.Fatalf("crypto symbols = %v", cr)
	}
}

