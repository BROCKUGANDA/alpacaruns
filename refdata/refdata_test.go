package refdata

// refdata_test.go: the snapshot must be able to say what changed, and must
// not treat a corrupt file as an empty universe.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/picker"
)

func inst(sym, class string, tradable bool) picker.Instrument {
	return picker.Instrument{Symbol: sym, Class: picker.Class(class), Tradable: tradable}
}

func TestDiffReportsEveryKindOfChange(t *testing.T) {
	prev := Build([]picker.Instrument{
		inst("AAPL", "equity", true),
		inst("OLD", "equity", true),
		inst("HALT", "equity", true),
		inst("MOVE", "equity", true),
	}, time.Now().Add(-time.Hour))
	next := Build([]picker.Instrument{
		inst("AAPL", "equity", true),
		inst("HALT", "equity", false), // halted
		inst("MOVE", "etf", true),     // reclassified
		inst("NEW", "equity", true),   // newly listed
	}, time.Now())

	changes := Diff(prev, next)
	got := map[ChangeKind]map[string]bool{}
	for _, c := range changes {
		if got[c.Kind] == nil {
			got[c.Kind] = map[string]bool{}
		}
		got[c.Kind][c.Symbol] = true
	}
	if !got[Disappeared]["OLD"] {
		t.Fatalf("a delisted symbol was not reported: %+v", changes)
	}
	if !got[Appeared]["NEW"] {
		t.Fatalf("a new symbol was not reported: %+v", changes)
	}
	if !got[Tradability]["HALT"] {
		t.Fatalf("a halt was not reported: %+v", changes)
	}
	if !got[ClassChanged]["MOVE"] {
		t.Fatalf("a reclassification was not reported: %+v", changes)
	}
	if got[Tradability]["AAPL"] || got[Appeared]["AAPL"] {
		t.Fatalf("an unchanged symbol was reported: %+v", changes)
	}
}

func TestSnapshotRoundTripAndVersioning(t *testing.T) {
	path := Path(filepath.Join(t.TempDir(), "trades.jsonl"))
	if snap, err := Load(path); err != nil || len(snap.Assets) != 0 {
		t.Fatalf("missing file should read as empty: %+v %v", snap, err)
	}
	snap := Build([]picker.Instrument{inst("AAPL", "equity", true)}, time.Now())
	saved, err := Save(path, snap)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if saved.Version != 1 {
		t.Fatalf("first save version = %d", saved.Version)
	}
	back, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if e, ok := back.Assets["AAPL"]; !ok || !e.Tradable || e.Class != "equity" {
		t.Fatalf("round trip lost data: %+v", back.Assets)
	}
	back.Version = 7
	if _, err := Save(path, back); err != nil {
		t.Fatalf("save 2: %v", err)
	}
	again, _ := Load(path)
	if again.Version != 7 {
		t.Fatalf("version not preserved: %d", again.Version)
	}
}

// A corrupt snapshot must be an error, not an empty universe: reading it as
// empty would report every symbol as newly appeared.
func TestCorruptSnapshotIsAnError(t *testing.T) {
	path := Path(filepath.Join(t.TempDir(), "trades.jsonl"))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("a corrupt snapshot loaded as valid")
	}
}

// A ticker change is detectable from broker data because the asset id is
// stable. It must be reported as a rename — not as one delisting plus one
// new listing, which would lose the link to the position's protection.
func TestDiffDetectsRenameByAssetID(t *testing.T) {
	prev := Build([]picker.Instrument{
		{Symbol: "FB", Class: "equity", AssetID: "asset-1", Tradable: true},
		{Symbol: "AAPL", Class: "equity", AssetID: "asset-2", Tradable: true},
	}, time.Now().Add(-time.Hour))
	next := Build([]picker.Instrument{
		{Symbol: "META", Class: "equity", AssetID: "asset-1", Tradable: true},
		{Symbol: "AAPL", Class: "equity", AssetID: "asset-2", Tradable: true},
	}, time.Now())

	changes := Diff(prev, next)
	var renamed *Change
	for i := range changes {
		switch changes[i].Kind {
		case Renamed:
			renamed = &changes[i]
		case Disappeared:
			t.Fatalf("a rename was also reported as a delisting: %s", changes[i].Symbol)
		case Appeared:
			t.Fatalf("a rename was also reported as a new listing: %s", changes[i].Symbol)
		}
	}
	if renamed == nil {
		t.Fatalf("rename not detected: %+v", changes)
	}
	if renamed.Symbol != "FB" || renamed.NewSymbol != "META" {
		t.Fatalf("wrong rename: %+v", renamed)
	}
	if renamed.Detail == "" {
		t.Fatal("rename carried no explanation")
	}
}

// Without an asset id the two events are genuinely indistinguishable, and the
// honest answer is still "disappeared + appeared" rather than a guessed pair.
func TestNoRenameWithoutAssetID(t *testing.T) {
	prev := Build([]picker.Instrument{{Symbol: "OLD", Class: "equity"}}, time.Now().Add(-time.Hour))
	next := Build([]picker.Instrument{{Symbol: "NEW", Class: "equity"}}, time.Now())
	for _, c := range Diff(prev, next) {
		if c.Kind == Renamed {
			t.Fatalf("invented a rename with no stable id: %+v", c)
		}
	}
}
