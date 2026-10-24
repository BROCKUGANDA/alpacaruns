package api

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/BROCKUGANDA/alpacaruns/factors"
	"github.com/BROCKUGANDA/alpacaruns/picker"
	"github.com/BROCKUGANDA/alpacaruns/tools"
)

// InstrumentsResponse is the body of GET /api/instruments: the
// classified universe — every instrument the bot may trade, with the
// asset class, venue, execution window and data family it needs.
type InstrumentsResponse struct {
	// Instruments is every classified instrument.
	Instruments []picker.Instrument `json:"instruments"`
	// Classes lists the classes present, with their counts.
	Classes map[string]int `json:"classes"`
}

// SignalsResponse is the body of GET /api/signals: the ranked
// candidates the picker produced, best score first.
type SignalsResponse struct {
	// Signals is the ranked candidate list.
	Signals []picker.Candidate `json:"signals"`
	// Scanned is how many instruments were considered.
	Scanned int `json:"scanned"`
	// Scored is how many produced a usable score.
	Scored int `json:"scored"`
}

// handleInstruments serves the classified universe. It is the
// read-only half of the universal signal picker: it answers "what can
// this bot trade, and how is each instrument classified" without
// needing a broker round-trip beyond the asset catalogue.
func (s *Server) handleInstruments(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return
	}
	reg, err := s.buildRegistry(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "registry_unavailable", err.Error())
		return
	}
	instruments := reg.Instruments()
	classes := map[string]int{}
	for _, i := range instruments {
		classes[string(i.Class)]++
	}
	if want := strings.TrimSpace(r.URL.Query().Get("class")); want != "" {
		filtered := instruments[:0:0]
		for _, i := range instruments {
			if string(i.Class) == want {
				filtered = append(filtered, i)
			}
		}
		instruments = filtered
	}
	writeJSON(w, http.StatusOK, InstrumentsResponse{Instruments: instruments, Classes: classes})
}

// handleSignals serves the ranked signals: every instrument scored
// with the factor engine and sorted best-first.
func (s *Server) handleSignals(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return
	}
	if s.client == nil {
		writeError(w, http.StatusServiceUnavailable, "no_broker", "no broker client is configured")
		return
	}
	reg, err := s.buildRegistry(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "registry_unavailable", err.Error())
		return
	}
	sc := &picker.Scanner{
		Client: s.client,
		Scorer: factors.NewEngine(s.cfg, picker.ClassBars{C: s.client}, s.client, factors.Options{}),
	}
	if s.cfg != nil {
		sc.MinComposite = s.cfg.FactorMinScore
	}
	cands, err := sc.Scan(r.Context(), reg)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "scan_failed", err.Error())
		return
	}
	scored := 0
	for _, c := range cands {
		if c.Error == "" {
			scored++
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].Composite > cands[j].Composite })
	writeJSON(w, http.StatusOK, SignalsResponse{Signals: cands, Scanned: len(cands), Scored: scored})
}

// assetCatalogueTTL is how long the broker's asset catalogue is cached.
// The catalogue is a large payload (every symbol Alpaca knows) that
// changes rarely, and re-fetching it on every dashboard poll would
// burn the API budget for no new information.
const assetCatalogueTTL = time.Hour

// assetCatalogue returns the broker's asset catalogue, cached for
// assetCatalogueTTL.
func (s *Server) assetCatalogue(ctx context.Context) (map[string]tools.Asset, error) {
	s.cfgMu.RLock()
	cached, at := s.catalogue, s.catalogueAt
	s.cfgMu.RUnlock()
	if cached != nil && time.Since(at) < assetCatalogueTTL {
		return cached, nil
	}
	if s.client == nil {
		return nil, nil
	}
	fresh, err := s.client.ListAssets(ctx)
	if err != nil {
		return nil, err
	}
	s.cfgMu.Lock()
	s.catalogue, s.catalogueAt = fresh, time.Now()
	s.cfgMu.Unlock()
	return fresh, nil
}

// buildRegistry assembles the classified universe from the configured
// symbol lists. Option underlyings are expanded into concrete
// contracts; a failure there is not fatal — the rest of the universe
// still scans.
func (s *Server) buildRegistry(ctx context.Context) (*picker.Registry, error) {
	spec := picker.Spec{}
	if s.strat != nil {
		spec.Equities = s.strat.EquitySymbols
		spec.ETFs = s.strat.ETFSymbols
		spec.Indices = s.strat.IndexSymbols
		spec.Crypto = s.strat.CryptoSymbols
		spec.OptionUnderlyings = s.strat.OptionUnderlyings
	}
	catalogue, err := s.assetCatalogue(ctx)
	if err != nil {
		return nil, err
	}
	reg := picker.BuildWithCatalogue(catalogue, spec)
	if len(spec.OptionUnderlyings) > 0 && s.client != nil {
		legs, err := picker.OptionLegs(ctx, s.client, spec)
		if err == nil {
			for _, l := range legs {
				reg.AddOption(l)
			}
		}
	}
	return reg, nil
}
