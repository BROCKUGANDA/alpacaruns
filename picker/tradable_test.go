package picker

import "testing"

// The trading loop must score only what the broker will trade; the picker
// page still shows the configured universe with its flags.
func TestTradableSymbolsExcludeWhatTheBrokerWillNotTrade(t *testing.T) {
	reg := &Registry{instruments: []Instrument{
		{Symbol: "AAPL", Class: ClassEquity, Tradable: true},
		{Symbol: "OLDCO", Class: ClassEquity, Tradable: false},
		{Symbol: "SPY", Class: ClassETF, Tradable: true},
		{Symbol: "BTC/USD", Class: ClassCrypto, Tradable: true},
	}}

	all := reg.Symbols()
	if len(all) != 4 {
		t.Fatalf("the configured universe should still list everything: %v", all)
	}
	trade := reg.TradableSymbols()
	if len(trade) != 3 {
		t.Fatalf("untradable symbol admitted to the trading universe: %v", trade)
	}
	for _, s := range trade {
		if s == "OLDCO" {
			t.Fatal("a delisted/untradable symbol was scored")
		}
	}
	bad := reg.Untradable()
	if len(bad) != 1 || bad[0].Symbol != "OLDCO" {
		t.Fatalf("exclusions must be reportable: %+v", bad)
	}
	// Class filters still apply.
	if got := reg.TradableSymbols(ClassCrypto); len(got) != 1 || got[0] != "BTC/USD" {
		t.Fatalf("class filter broken: %v", got)
	}
}
