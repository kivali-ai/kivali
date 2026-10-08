package web

import (
	"math"
	"sort"
	"sync"
	"time"

	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// The usage rollup behind the Org page's usage section and the
// settings dashboard: spend tiles, spend per UTC day, spend per agent,
// and the calls-and-tokens table, all from one cached read of
// usage.jsonl per usage version.

// usageRollupWindow is how much history the rollup cache holds: the
// 30-day figures plus a day of slack. The cache is a superset of every
// later request's window until the next append replaces it, because
// the windows only move forward with the clock.
const usageRollupWindow = 31 * 24 * time.Hour

// usageDays is how many bars "Spend per day" has.
const usageDays = 30

// usageNoAgentName names the calls made for no agent: the inbox and
// project-file summaries Kivali runs for itself.
const usageNoAgentName = "Kivali"

// priceUsage prices one usage row from its token counts at the
// provider's rates (Provider.Price), NEVER from the stored CostUSD, which is
// not summable (see store.UsageRecord.CostUSD). ok is false for a model
// with no price: the row adds nothing to spend and its tokens are
// reported as unpriced rather than as free. Home's spend readouts
// (pricedUsageWindow) price the same way, so the two pages agree.
func priceUsage(p provider.Provider, u store.UsageRecord) (cost float64, ok bool) {
	if _, known := p.Resolve(u.Model); !known {
		return 0, false
	}
	return p.Price(u.Model, provider.TokenUsage{
		InputTokens:       u.InputTokens,
		OutputTokens:      u.OutputTokens,
		CacheReadTokens:   u.CacheReadTokens,
		CacheCreateTokens: u.CacheCreateTokens,
	}), true
}

// rolledUsage is one usage row reduced to what the rollup sums.
type rolledUsage struct {
	TS          time.Time
	Agent       string
	Input       int // fresh input only
	Output      int
	CacheRead   int
	CacheCreate int
	Cost        float64
	Priced      bool
}

// tokensIn is everything sent to the model: the fresh prompt plus both
// cache legs, since a cache read or write is still context fed in.
func (u rolledUsage) tokensIn() int { return u.Input + u.CacheRead + u.CacheCreate }

// usageRollupCache holds the rolled rows, tagged with the usage
// version they were read under. The sums are recomputed per request
// from the cached rows, since every window moves with the clock even
// when nothing is appended.
type usageRollupCache struct {
	mu      sync.Mutex
	version int64
	loaded  bool
	rows    []rolledUsage
}

// usageRollupCache is the Server's cache (Server.usageRollupRows); its
// zero value is empty, so no init step is needed.
func (s *Server) usageRollupCache() *usageRollupCache {
	return &s.usageRollupRows
}

// usageRows returns the rolled rows of the last usageRollupWindow,
// rereading usage.jsonl only when an append (or a restore) has bumped
// the usage version since they were read. The version is read before
// the file, so a write landing in between leaves the cache one version
// behind, never ahead. A read failure caches nothing.
func (s *Server) usageRows(now time.Time) ([]rolledUsage, error) {
	version := s.usageVersion.Load()
	c := s.usageRollupCache()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loaded && c.version == version {
		return c.rows, nil
	}
	records, err := s.Store.ReadUsageSince(now.Add(-usageRollupWindow))
	if err != nil {
		return nil, err
	}
	rows := make([]rolledUsage, 0, len(records))
	for _, u := range records {
		cost, priced := priceUsage(s.Provider, u)
		rows = append(rows, rolledUsage{
			TS: u.TS, Agent: u.Agent,
			Input: u.InputTokens, Output: u.OutputTokens,
			CacheRead: u.CacheReadTokens, CacheCreate: u.CacheCreateTokens,
			Cost: cost, Priced: priced,
		})
	}
	c.rows, c.version, c.loaded = rows, version, true
	return rows, nil
}

// usageTileSum accumulates one spend tile.
type usageTileSum struct {
	spend             float64
	calls             int
	cacheRead, inside int
}

func (t *usageTileSum) add(u rolledUsage) {
	t.spend += u.Cost
	t.calls++
	t.cacheRead += u.CacheRead
	t.inside += u.tokensIn()
}

func (t usageTileSum) tile() apitypes.UsageTile {
	pct := 0
	if t.inside > 0 {
		pct = int(math.Round(100 * float64(t.cacheRead) / float64(t.inside)))
	}
	return apitypes.UsageTile{Spend: t.spend, Calls: t.calls, CacheHitPct: pct}
}

func addUsageWindow(w *apitypes.UsageWindow, u rolledUsage) {
	w.Calls++
	w.TokensIn += u.tokensIn()
	w.TokensOut += u.Output
	w.CacheRead += u.CacheRead
	w.CacheCreate += u.CacheCreate
	w.Spend += u.Cost
	if !u.Priced {
		w.Unpriced += u.tokensIn() + u.Output
		w.UnpricedCalls++
	}
}

// usageRollup computes the Org page's usage figures at now. name
// labels an agent slug for by_agent; it is never called with "".
func (s *Server) usageRollup(now time.Time, name func(slug string) string) (apitypes.Usage, error) {
	rows, err := s.usageRows(now)
	if err != nil {
		return apitypes.Usage{}, err
	}
	return rollUsage(rows, now, name), nil
}

// rollUsage is usageRollup over a given set of rows.
func rollUsage(rows []rolledUsage, now time.Time, name func(slug string) string) apitypes.Usage {
	now = now.UTC()
	today := now.Truncate(24 * time.Hour)
	cut24h := now.Add(-24 * time.Hour)
	cut7d := now.Add(-7 * 24 * time.Hour)
	cut30d := now.Add(-30 * 24 * time.Hour)
	firstDay := today.AddDate(0, 0, -(usageDays - 1))

	daily := make([]apitypes.UsageDay, usageDays)
	dayIndex := make(map[string]int, usageDays)
	for i := range daily {
		d := firstDay.AddDate(0, 0, i).Format(time.DateOnly)
		daily[i] = apitypes.UsageDay{Date: d}
		dayIndex[d] = i
	}

	var tToday, t7d, t30d usageTileSum
	var windows apitypes.UsageWindows
	type agentSpend struct{ d7, d30 float64 }
	byAgent := map[string]*agentSpend{}

	for _, u := range rows {
		if u.TS.After(now) {
			continue
		}
		if i, ok := dayIndex[u.TS.UTC().Format(time.DateOnly)]; ok {
			daily[i].Spend += u.Cost
		}
		if u.TS.Before(cut30d) {
			continue
		}
		t30d.add(u)
		addUsageWindow(&windows.D30, u)
		a := byAgent[u.Agent]
		if a == nil {
			a = &agentSpend{}
			byAgent[u.Agent] = a
		}
		a.d30 += u.Cost
		if !u.TS.Before(cut7d) {
			t7d.add(u)
			addUsageWindow(&windows.D7, u)
			a.d7 += u.Cost
		}
		if !u.TS.Before(cut24h) {
			addUsageWindow(&windows.H24, u)
		}
		if !u.TS.Before(today) {
			tToday.add(u)
		}
	}

	agents := make([]apitypes.UsageAgent, 0, len(byAgent))
	for slug, a := range byAgent {
		n := usageNoAgentName
		if slug != "" {
			n = name(slug)
		}
		agents = append(agents, apitypes.UsageAgent{Slug: slug, Name: n, D7: a.d7, D30: a.d30})
	}
	sort.Slice(agents, func(i, j int) bool {
		if agents[i].D30 != agents[j].D30 {
			return agents[i].D30 > agents[j].D30
		}
		return agents[i].Slug < agents[j].Slug
	})

	return apitypes.Usage{
		Tiles:   apitypes.UsageTiles{Today: tToday.tile(), D7: t7d.tile(), D30: t30d.tile()},
		Daily:   daily,
		ByAgent: agents,
		Windows: windows,
	}
}
