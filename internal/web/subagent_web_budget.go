// Package web (subagent_web_budget.go) — how much data a web task may
// send out.
//
// A web task's browser reaches the public web, and every web_* call
// passes through core, so core counts what a task could carry out in
// it: the data in the URLs it goes to (a long path and query, a long
// host name, user info, a long fragment, and every byte of them once a
// task's free pool is spent), the text it types, the keys it presses,
// the values it selects, the text it waits for and the scripts it
// runs, plus its start URL at dispatch. Element refs, tab ids and the names
// of a task's own files are not counted, nor is anything a person does
// in the browser window. The limits are loose: an ordinary task never
// comes near them, so going over almost always means a page has led
// the task to send out data. An action over a limit is refused with a
// reason the model reads, and the first refusal in a task posts one
// notice to Needs you. A task whose uploads a person approved is
// counted like any other: the approval covers the upload, not what the
// task types or sends on that site.
//
// The counters are in memory, on the service's clock: a restart starts
// them afresh, as it ends every task in flight.

package web

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kivali-ai/kivali/internal/browserpod"
)

// Web budget limits, in bytes.
const (
	webBudgetPerAction = 2 << 10
	webBudgetPerTask   = 8 << 10
	webBudgetPerDay    = 32 << 10
	// webBudgetWindow is the rolling window the per-agent limit counts.
	webBudgetWindow = 24 * time.Hour
	// A URL's path and query together are free up to webURLFreePath
	// bytes, its host name up to webURLFreeHost, and its fragment when
	// it is no longer than webURLLongFragment: ordinary pages, ids and
	// searches cost nothing. Beyond that every byte counts, however it
	// is split into segments, labels or parameters.
	webURLFreePath     = 64
	webURLFreeHost     = 48
	webURLLongFragment = 64
	// webURLFreePool is how many bytes those allowances may waive over
	// a task's whole run: once it is spent, every byte of a URL's host,
	// path and query counts, so many small URLs cannot carry data free.
	webURLFreePool = 1 << 10
)

// webBudgetLimit names the limit an action went over.
type webBudgetLimit string

const (
	webBudgetLimitAction webBudgetLimit = "action"
	webBudgetLimitTask   webBudgetLimit = "task"
	webBudgetLimitDay    webBudgetLimit = "day"
)

// webBudget is the service's counters and notices. Guarded by its own
// lock, never held with jobsMu.
type webBudget struct {
	mu sync.Mutex
	// spent is each agent's charges in the rolling window, oldest
	// first.
	spent map[string][]webSpend
	// tasks is what each task has sent, by job id.
	tasks map[string]int
	// free is how much of each task's webURLFreePool its URLs have used.
	free map[string]int
	// notices are the posted notices by job id; acked ones stay, so a
	// task posts at most one.
	notices map[string]*webBudgetNotice
}

// webSpend is one charge against an agent's day.
type webSpend struct {
	at    time.Time
	bytes int
}

// webBudgetNotice is what Needs you says about a task that went over:
// the agent, the task, the site and the size of the refused action.
type webBudgetNotice struct {
	Parent      string
	ID          string
	Description string
	Site        string
	Bytes       int
	Limit       webBudgetLimit
	At          time.Time
	acked       bool
}

// webURLCost is the data a URL carries out: its path and query
// (decoded) beyond the free allowance they share, its host name beyond
// its allowance, its user info, and its fragment when it is long. A
// URL that does not parse counts in full.
func webURLCost(raw string) int {
	cost, _ := webURLCharge(raw)
	return cost
}

// webURLCharge is webURLCost and the bytes the allowances waived, which
// the task's free pool pays for.
func webURLCharge(raw string) (cost, waived int) {
	if raw == "" {
		return 0, 0
	}
	u, err := url.Parse(raw)
	if err != nil {
		return len(raw), 0
	}
	if u.User != nil {
		cost += len(u.User.String())
	}
	host := len(u.Hostname())
	cost += max(0, host-webURLFreeHost)
	waived += min(host, webURLFreeHost)
	pathQuery := len(u.Path)
	for _, pair := range strings.FieldsFunc(u.RawQuery, func(r rune) bool { return r == '&' || r == ';' }) {
		k, v, _ := strings.Cut(pair, "=")
		pathQuery += unescapedLen(k, url.QueryUnescape) + unescapedLen(v, url.QueryUnescape)
	}
	cost += max(0, pathQuery-webURLFreePath)
	waived += min(pathQuery, webURLFreePath)
	if len(u.Fragment) > webURLLongFragment {
		cost += len(u.Fragment)
	}
	return cost, waived
}

// unescapedLen is s's length once unescaped, or its raw length when it
// does not unescape.
func unescapedLen(s string, unescape func(string) (string, error)) int {
	if d, err := unescape(s); err == nil {
		return len(d)
	}
	return len(s)
}

// webActionCost is what one web command sends out, by its arguments,
// before the task's free pool.
func webActionCost(command string, args json.RawMessage) int {
	cost, _ := webActionCharge(command, args)
	return cost
}

// webActionCharge is webActionCost and, for a URL, the bytes its
// allowances waived. Arguments that do not decode cost nothing here:
// the pod decodes them the same way and refuses them.
func webActionCharge(command string, args json.RawMessage) (cost, waived int) {
	switch command {
	case browserpod.CommandNavigate:
		var a browserpod.NavigateArgs
		if json.Unmarshal(args, &a) == nil {
			return webURLCharge(strings.TrimSpace(a.URL))
		}
	case browserpod.CommandTabs:
		var a browserpod.TabsArgs
		if json.Unmarshal(args, &a) == nil {
			return webURLCharge(strings.TrimSpace(a.URL))
		}
	case browserpod.CommandType:
		var a browserpod.TypeArgs
		if json.Unmarshal(args, &a) == nil {
			return len(a.Text), 0
		}
	case browserpod.CommandPress:
		var a browserpod.PressArgs
		if json.Unmarshal(args, &a) == nil {
			return len(a.Key), 0
		}
	case browserpod.CommandSelect:
		var a browserpod.SelectArgs
		if json.Unmarshal(args, &a) == nil {
			return len(a.Value), 0
		}
	case browserpod.CommandEval:
		var a browserpod.EvalArgs
		if json.Unmarshal(args, &a) == nil {
			return len(a.Script), 0
		}
	case browserpod.CommandWait:
		var a browserpod.WaitArgs
		if json.Unmarshal(args, &a) == nil && a.For == browserpod.WaitText {
			return len(a.Value), 0
		}
	}
	return 0, 0
}

// byteSize is a size for people and models: "640 bytes", "2 KB",
// "5.1 KB".
func byteSize(n int) string {
	if n < 1<<10 {
		if n == 1 {
			return "1 byte"
		}
		return fmt.Sprintf("%d bytes", n)
	}
	kb := fmt.Sprintf("%.1f", float64(n)/(1<<10))
	return strings.TrimSuffix(kb, ".0") + " KB"
}

// spentLocked is what an agent has sent in the window ending at now,
// dropping charges older than it. b.mu is held.
func (b *webBudget) spentLocked(parent string, now time.Time) int {
	charges := b.spent[parent]
	cut := 0
	for cut < len(charges) && !charges[cut].at.After(now.Add(-webBudgetWindow)) {
		cut++
	}
	charges = charges[cut:]
	if len(charges) == 0 {
		delete(b.spent, parent)
	} else {
		b.spent[parent] = charges
	}
	total := 0
	for _, c := range charges {
		total += c.bytes
	}
	return total
}

// chargeLocked records bytes against a task and its agent. b.mu is
// held.
func (b *webBudget) chargeLocked(parent, id string, bytes int, now time.Time) {
	if bytes == 0 {
		return
	}
	if b.spent == nil {
		b.spent = map[string][]webSpend{}
		b.tasks = map[string]int{}
	}
	b.spent[parent] = append(b.spent[parent], webSpend{at: now, bytes: bytes})
	b.tasks[id] += bytes
}

// webBudgetFits refuses a batch whose start URLs together would take
// parent past its day.
func (s *SubagentService) webBudgetFits(parent string, costs []int) error {
	total := 0
	for _, c := range costs {
		total += c
	}
	if total == 0 {
		return nil
	}
	b := &s.webBudget
	b.mu.Lock()
	spent := b.spentLocked(parent, s.clk().Now())
	b.mu.Unlock()
	if spent+total > webBudgetPerDay {
		return fmt.Errorf("the start URLs carry %s of data in their queries and long path parts, and your web tasks have sent %s in the last 24 hours, past the %s a day web tasks may send out; give the tasks plain URLs", byteSize(total), byteSize(spent), byteSize(webBudgetPerDay))
	}
	return nil
}

// poolLocked is what an action costs once the task's free pool has paid
// for what it can of the waived bytes, and how much of the pool that
// takes. b.mu is held; nothing is recorded.
func (b *webBudget) poolLocked(id string, cost, waived int) (bytes, use int) {
	use = min(waived, max(0, webURLFreePool-b.free[id]))
	return cost + waived - use, use
}

// usePoolLocked records use of a task's free pool. b.mu is held.
func (b *webBudget) usePoolLocked(id string, use int) {
	if use == 0 {
		return
	}
	if b.free == nil {
		b.free = map[string]int{}
	}
	b.free[id] += use
}

// chargeWebStart counts a task's start URL as it is registered.
func (s *SubagentService) chargeWebStart(parent, id, startURL string) {
	b := &s.webBudget
	b.mu.Lock()
	defer b.mu.Unlock()
	cost, waived := webURLCharge(startURL)
	bytes, use := b.poolLocked(id, cost, waived)
	b.usePoolLocked(id, use)
	b.chargeLocked(parent, id, bytes, s.clk().Now())
}

// chargeWebAction counts one web command of a running task against the
// budget. Within it, the command is charged and "" comes back; over
// it, nothing is charged, the task's first breach posts a notice, and
// the refusal the model reads comes back. site names where the data
// would have gone, for the notice.
func (s *SubagentService) chargeWebAction(j subagentJob, command string, args json.RawMessage, site func() string) string {
	cost, waived := webActionCharge(command, args)
	if cost == 0 && waived == 0 {
		return ""
	}
	now := s.clk().Now()
	b := &s.webBudget
	b.mu.Lock()
	bytes, use := b.poolLocked(j.ID, cost, waived)
	var limit webBudgetLimit
	var refusal string
	spent := b.spentLocked(j.Parent, now)
	task := b.tasks[j.ID]
	tool := "web_" + command
	switch {
	case bytes > webBudgetPerAction:
		limit = webBudgetLimitAction
		refusal = fmt.Sprintf("Refused: this %s sends %s, and one action may send at most %s.", tool, byteSize(bytes), byteSize(webBudgetPerAction))
	case task+bytes > webBudgetPerTask:
		limit = webBudgetLimitTask
		refusal = fmt.Sprintf("Refused: this %s sends %s, which would take this task past the %s it may send out (it has sent %s).", tool, byteSize(bytes), byteSize(webBudgetPerTask), byteSize(task))
	case spent+bytes > webBudgetPerDay:
		limit = webBudgetLimitDay
		refusal = fmt.Sprintf("Refused: this %s sends %s, which would take your caller's web tasks past the %s a day they may send out (they have sent %s in the last 24 hours).", tool, byteSize(bytes), byteSize(webBudgetPerDay), byteSize(spent))
	default:
		b.usePoolLocked(j.ID, use)
		b.chargeLocked(j.Parent, j.ID, bytes, now)
		b.mu.Unlock()
		return ""
	}
	_, noticed := b.notices[j.ID]
	b.mu.Unlock()
	if !noticed && s.postWebBudgetNotice(&webBudgetNotice{Parent: j.Parent, ID: j.ID, Description: j.Description, Site: site(), Bytes: bytes, Limit: limit, At: now}) {
		s.notifyWorkingChanged()
	}
	return refusal + " Web tasks have a budget on what they send out: the text you type, the data in the URLs you go to, keys, selected values and scripts. A person has been told. Carry on without this action if you can; otherwise submit what you have, or blocked."
}

// postWebBudgetNotice keeps n unless its task has posted one already,
// reporting whether it did. The site was looked up outside the lock,
// since it may ask the browser registry.
func (s *SubagentService) postWebBudgetNotice(n *webBudgetNotice) bool {
	b := &s.webBudget
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.notices[n.ID]; ok {
		return false
	}
	if b.notices == nil {
		b.notices = map[string]*webBudgetNotice{}
	}
	b.notices[n.ID] = n
	return true
}

// forgetWebTask drops a finished task's count. Its notice stays until
// a person acknowledges it.
func (s *SubagentService) forgetWebTask(id string) {
	b := &s.webBudget
	b.mu.Lock()
	delete(b.tasks, id)
	delete(b.free, id)
	b.mu.Unlock()
}

// webBudgetNotices are the notices nobody has acknowledged, oldest
// first, for Home's needs.
func (s *SubagentService) webBudgetNotices() []webBudgetNotice {
	b := &s.webBudget
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []webBudgetNotice
	for _, n := range b.notices {
		if !n.acked {
			out = append(out, *n)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].At.Equal(out[j].At) {
			return out[i].ID < out[j].ID
		}
		return out[i].At.Before(out[j].At)
	})
	return out
}

// ackWebBudgetNotice clears one of parent's notices from Needs you,
// reporting whether there was one to clear.
func (s *SubagentService) ackWebBudgetNotice(parent, id string) bool {
	b := &s.webBudget
	b.mu.Lock()
	n, ok := b.notices[id]
	cleared := ok && n.Parent == parent && !n.acked
	if cleared {
		n.acked = true
	}
	b.mu.Unlock()
	if cleared {
		s.notifyWorkingChanged()
	}
	return cleared
}
