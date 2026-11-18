package main

import (
	"cmp"
	"log"
	"math"
	"net/http"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const version = "1.3.1"

const (
	pluginID       = "reset-first"
	limitRecheck   = 5 * time.Minute
	probeRetry     = 15 * time.Minute
	resetTolerance = 10 * time.Minute
)

const (
	groupRecheck = iota
	groupProbe
	groupKnown
	groupPending
	groupOther
	groupLimited
)

var quotaReaders = map[string]func(headers map[string][]string, requestedAt, now time.Time) quota{
	"claude": claudeQuota,
	"codex":  codexQuota,
}

var claudeWindows = []struct {
	name    string
	model   string
	minutes int64
}{
	{"5h", "", 5 * 60},
	{"7d", "", 7 * 24 * 60},
	{"7d_oi", "fable", 7 * 24 * 60},
}

// scopedProvider is the only provider whose accounts report a model's own limit.
const scopedProvider = "claude"

// limitScopes lists "" for the limits every request counts against, then each model with a limit of its own, such as
// Fable's weekly limit, which only that model's requests count against.
var limitScopes = func() []string {
	scopes := []string{""}

	for _, claim := range claudeWindows {
		if !slices.Contains(scopes, claim.model) {
			scopes = append(scopes, claim.model)
		}
	}

	return scopes
}()

var resetLogger = log.New(os.Stderr, pluginID+": ", 0)

type quota struct {
	reset   int64
	windows []observation
}

type window struct {
	minutes    int64
	model      string
	used       float64
	reset      int64
	limited    bool
	observedAt time.Time
	since      time.Time
}

type observation struct {
	window
	earliest int64
	// allowed marks a window reported allowed without its usage, which Anthropic leaves out when a window is unused. A
	// warning status says usage is high, so it doesn't count.
	allowed bool
}

type account struct {
	provider     string
	priority     int
	subscription bool
	seen         bool
	reset        int64
	probedAt     time.Time
	checkedAt    map[string]time.Time
	windows      []window
}

// offer names the candidates CPA last offered for a provider's requests in one scope: "" for models without a limit
// of their own, or a model's scope, such as "fable". CPA can leave an account out of one model's requests only.
type offer struct {
	provider string
	scope    string
}

type router struct {
	mu       sync.Mutex
	now      func() time.Time
	accounts map[string]account
	offered  map[offer]map[string]bool
	ever     map[offer]map[string]bool
	aliases  map[string]string
	serving  map[string]string
}

type statusReport struct {
	Version   string           `json:"version"`
	Now       time.Time        `json:"now"`
	Providers []providerReport `json:"providers"`
}

type providerReport struct {
	Provider string `json:"provider"`
	Serving  string `json:"serving,omitempty"`
	// Models maps each model with a limit of its own, such as "fable", to the account its requests go to, or to ""
	// when none of the accounts CPA offers can take them.
	Models map[string]string `json:"models,omitempty"`
	// ModelsUntil gives, for a model whose requests no offered account can take, when the first of those accounts frees up.
	ModelsUntil map[string]time.Time `json:"models_until,omitempty"`
	Accounts    []accountReport      `json:"accounts"`
}

type accountReport struct {
	ID           string         `json:"id"`
	State        string         `json:"state"`
	Available    bool           `json:"available"`
	Priority     int            `json:"priority,omitempty"`
	ResetsAt     *time.Time     `json:"resets_at,omitempty"`
	RetryAt      *time.Time     `json:"retry_at,omitempty"`
	LimitedUntil *time.Time     `json:"limited_until,omitempty"`
	Windows      []windowReport `json:"windows,omitempty"`
}

type windowReport struct {
	Minutes     int64      `json:"minutes"`
	Model       string     `json:"model,omitempty"`
	UsedPercent float64    `json:"used_percent"`
	ResetsAt    *time.Time `json:"resets_at,omitempty"`
	Limited     bool       `json:"limited,omitempty"`
}

func newRouter(now func() time.Time) *router {
	return &router{
		now:      now,
		accounts: make(map[string]account),
		offered:  make(map[offer]map[string]bool),
		ever:     make(map[offer]map[string]bool),
		aliases:  make(map[string]string),
		serving:  make(map[string]string),
	}
}

func (r *router) Observe(record pluginapi.UsageRecord) {
	provider := normalizeProvider(record.Provider)

	readQuota, ok := quotaReaders[provider]
	if !ok || record.AuthID == "" {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if !record.Failed {
		r.serving[provider] = record.AuthID
	}

	// A model alias hides which model a request is for, so each requested name keeps the scope of the model it resolved to.
	if name := strings.ToLower(strings.TrimSpace(record.Alias)); provider == scopedProvider && name != "" && record.Model != "" {
		r.aliases[name] = scopeOf(record.Model)
	}

	now := r.now()
	state := r.accounts[record.AuthID]
	state.provider = provider

	// A request time after now means the clock moved back, so it cannot order responses.
	requestedAt := record.RequestedAt
	if requestedAt.After(now) {
		requestedAt = time.Time{}
	}

	observed := readQuota(record.ResponseHeaders, requestedAt, now)

	// A model's own window that came without any reset takes the account's reset, as Claude's Fable window resets with its
	// weekly window, unless the request went out before that window began and the response belongs to the one before.
	for i, w := range observed.windows {
		if w.model != "" && w.reset == 0 && state.reset > now.Unix() && requestedAt.Unix() >= state.reset-w.minutes*60 {
			observed.windows[i].reset, observed.windows[i].earliest = state.reset, state.reset
		}
	}

	before := make([]int64, len(limitScopes))
	for i, scope := range limitScopes {
		before[i] = state.limitedUntil(now, scope)
	}

	state.windows = mergeWindows(state.windows, observed.windows, requestedAt, now)

	for i, scope := range limitScopes {
		reached, cleared := "a quota limit", "its quota limits"
		if scope != "" {
			reached, cleared = "its "+scope+" limit", "its "+scope+" limit"
		}

		switch until := state.limitedUntil(now, scope); {
		case until > 0 && before[i] == 0:
			state.check(scope, now)
			resetLogger.Printf("auth %q reached %s until %s", record.AuthID, reached, time.Unix(until, 0).UTC().Format(time.RFC3339))
		case until == 0 && before[i] > 0:
			resetLogger.Printf("auth %q is below %s again", record.AuthID, cleared)
		}
	}

	tolerance := int64(resetTolerance / time.Second)

	switch reset := observed.reset; {
	case reset <= now.Unix():
		if state.reset <= now.Unix() {
			state.probedAt = now
		}
	case reset < state.reset-tolerance || reset > state.reset+tolerance:
		state.reset = reset
		state.probedAt = time.Time{}
		resetLogger.Printf("auth %q quota resets %s", record.AuthID, time.Unix(reset, 0).UTC().Format(time.RFC3339))
	}

	r.accounts[record.AuthID] = state
}

func (r *router) Pick(req pluginapi.SchedulerPickRequest) pluginapi.SchedulerPickResponse {
	provider, subscriptions := pool(req.Candidates)
	if provider == "" {
		return pluginapi.SchedulerPickResponse{}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	offered := make(map[string]bool, len(req.Candidates))

	for _, candidate := range req.Candidates {
		state := r.accounts[candidate.ID]
		state.provider = provider
		state.priority = candidate.Priority
		state.subscription = isSubscription(candidate)
		state.seen = true
		r.accounts[candidate.ID] = state
		offered[candidate.ID] = true
	}

	scope := r.requestScope(provider, req.Model)

	// A token count reports no usage and can fail where a message wouldn't, so it neither checks a limit nor changes
	// the offers the status page shows.
	path, _ := req.Options.Metadata["request_path"].(string)
	counting := strings.HasSuffix(path, "/count_tokens")

	if !counting {
		r.offered[offer{provider, scope}] = offered
	}

	// A retry offers only the accounts not tried yet, so the accounts ever offered for a model's requests are kept too.
	if scope != "" && !counting {
		ever := r.ever[offer{provider, scope}]
		if ever == nil {
			ever = make(map[string]bool)
			r.ever[offer{provider, scope}] = ever
		}

		for id := range offered {
			ever[id] = true
		}
	}

	if !subscriptions {
		return pluginapi.SchedulerPickResponse{}
	}

	now := r.now()

	chosen := req.Candidates[0]
	for _, candidate := range req.Candidates[1:] {
		if r.less(candidate, chosen, now, scope, !counting) {
			chosen = candidate
		}
	}

	// This request shows whether the limits it counts against still hold, so the next check of each waits.
	if !counting {
		state := r.accounts[chosen.ID]
		for _, s := range scopesFor(scope) {
			if state.limitedUntil(now, s) > 0 {
				state.check(s, now)
				r.accounts[chosen.ID] = state
			}
		}
	}

	return pluginapi.SchedulerPickResponse{Handled: true, AuthID: chosen.ID}
}

func (r *router) Status() statusReport {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()

	pools := make(map[string][]pluginapi.SchedulerAuthCandidate)
	for id, state := range r.accounts {
		if state.seen {
			pools[state.provider] = append(pools[state.provider], state.candidate(id))
		}
	}

	providers := make([]string, 0, len(pools))
	for provider := range pools {
		providers = append(providers, provider)
	}

	sort.Strings(providers)

	report := statusReport{Version: version, Now: now.UTC(), Providers: make([]providerReport, 0, len(providers))}

	for _, provider := range providers {
		candidates := pools[provider]

		// The order shown is for requests that count only against the shared limits. Until one arrives, which accounts
		// CPA offers for them is unknown, so every account counts as offered.
		offered, known := r.offered[offer{provider, ""}]
		available := func(id string) bool { return !known || offered[id] }

		sort.Slice(candidates, func(i, j int) bool {
			left, right := candidates[i], candidates[j]

			if available(left.ID) != available(right.ID) {
				return available(left.ID)
			}

			if left.Priority != right.Priority {
				return left.Priority > right.Priority
			}

			return r.less(left, right, now, "", true)
		})

		pool := providerReport{Provider: provider, Serving: r.serving[provider], Accounts: make([]accountReport, 0, len(candidates))}
		pool.Models, pool.ModelsUntil = r.destinations(provider, candidates, available, now)

		for _, candidate := range candidates {
			pool.Accounts = append(pool.Accounts, r.accountReport(candidate, available(candidate.ID), now))
		}

		report.Providers = append(report.Providers, pool)
	}

	return report
}

// destinations returns, for each model with a limit of its own that the provider's requests or accounts have shown,
// the account its requests go to while no check is due, as Pick ranks the candidates CPA last offered for that model.
// It is "" when none of them can take those requests. Then the second map says when the first of them frees up, unless
// CPA leaves out an account that has nothing holding it, which means a pause in CPA with no known end.
func (r *router) destinations(provider string, candidates []pluginapi.SchedulerAuthCandidate, available func(string) bool, now time.Time) (map[string]string, map[string]time.Time) {
	var models map[string]string
	var untils map[string]time.Time

	for _, scope := range limitScopes[1:] {
		reported := slices.ContainsFunc(candidates, func(candidate pluginapi.SchedulerAuthCandidate) bool {
			return slices.ContainsFunc(r.accounts[candidate.ID].windows, func(w window) bool { return w.model == scope })
		})

		offeredFor, requested := r.offered[offer{provider, scope}]
		if !requested && !reported {
			continue
		}

		ever := r.ever[offer{provider, scope}]

		var chosen *pluginapi.SchedulerAuthCandidate
		until, paused := int64(0), false

		for i, candidate := range candidates {
			group, at := r.rank(candidate, now, scope, true)
			held := group == groupLimited || group == groupRecheck
			offeredHere := requested && offeredFor[candidate.ID] || !requested && available(candidate.ID)

			// A retry leaves out the accounts CPA already tried, so a held account counts toward when these requests can
			// go again if CPA ever offered it for them. An account CPA never offers for them can't take them.
			if held && (offeredHere || ever[candidate.ID]) {
				if until == 0 || at < until {
					until = at
				}

				continue
			}

			if !offeredHere {
				paused = paused || ever[candidate.ID] && !held
				continue
			}

			if chosen == nil || r.less(candidate, *chosen, now, scope, true) {
				chosen = &candidates[i]
			}
		}

		if models == nil {
			models = make(map[string]string)
		}

		models[scope] = ""
		if chosen != nil {
			models[scope] = chosen.ID
			continue
		}

		if until > 0 && !paused {
			if untils == nil {
				untils = make(map[string]time.Time)
			}

			untils[scope] = time.Unix(until, 0).UTC()
		}
	}

	return models, untils
}

func (r *router) accountReport(candidate pluginapi.SchedulerAuthCandidate, available bool, now time.Time) accountReport {
	state := r.accounts[candidate.ID]
	report := accountReport{ID: candidate.ID, Available: available, Priority: state.priority, Windows: state.windowReports(now)}

	if state.reset > now.Unix() {
		resetsAt := time.Unix(state.reset, 0).UTC()
		report.ResetsAt = &resetsAt
	}

	switch group, until := r.rank(candidate, now, "", true); group {
	case groupLimited, groupRecheck:
		limitedUntil := time.Unix(until, 0).UTC()
		report.State, report.LimitedUntil = "limited", &limitedUntil
	case groupKnown:
		report.State = "known"
	case groupProbe:
		report.State = "learning"
	case groupPending:
		retryAt := state.probedAt.Add(probeRetry).UTC()
		report.State, report.RetryAt = "waiting", &retryAt
	default:
		report.State = "api_key"
	}

	return report
}

func (a account) candidate(id string) pluginapi.SchedulerAuthCandidate {
	kind := "apikey"
	if a.subscription {
		kind = "oauth"
	}

	return pluginapi.SchedulerAuthCandidate{ID: id, Provider: a.provider, Priority: a.priority, Attributes: map[string]string{"auth_kind": kind}}
}

// limitedUntil returns when the limits of one scope stop holding the account. Scope "" is the limits every request
// counts against; a model's scope, such as "fable", is the limit only that model's requests count against.
func (a account) limitedUntil(now time.Time, scope string) int64 {
	until := int64(0)

	for _, w := range a.windows {
		if w.model == scope && w.limited && w.reset > now.Unix() && w.reset > until {
			until = w.reset
		}
	}

	return until
}

// limit returns when the limits a request in the scope counts against stop holding the account, and when a request
// last checked them.
func (a account) limit(now time.Time, scope string) (int64, time.Time) {
	until, checkedAt := int64(0), time.Time{}

	for _, s := range scopesFor(scope) {
		if at := a.limitedUntil(now, s); at > 0 {
			until = max(until, at)

			if a.checkedAt[s].After(checkedAt) {
				checkedAt = a.checkedAt[s]
			}
		}
	}

	return until, checkedAt
}

func (a *account) check(scope string, now time.Time) {
	if a.checkedAt == nil {
		a.checkedAt = make(map[string]time.Time)
	}

	a.checkedAt[scope] = now
}

// scopeOf returns the scope of the limit a request for the model counts against besides the shared ones, such as
// "fable" for a Fable model, or "" when it counts only against the shared ones.
func scopeOf(model string) string {
	model = strings.ToLower(model)

	for _, scope := range limitScopes[1:] {
		if strings.Contains(model, scope) {
			return scope
		}
	}

	return ""
}

// requestScope returns the scope of a request for the model name it asked for. A name already seen resolving to a model
// takes that model's scope, since an alias can hide the model or name another one.
func (r *router) requestScope(provider, model string) string {
	if provider != scopedProvider {
		return ""
	}

	if scope, learned := r.aliases[strings.ToLower(strings.TrimSpace(model))]; learned {
		return scope
	}

	return scopeOf(model)
}

// scopesFor lists the scopes whose limits a request in the scope counts against: the shared ones, and its own.
func scopesFor(scope string) []string {
	if scope == "" {
		return []string{""}
	}

	return []string{"", scope}
}

func mergeWindows(stored []window, observed []observation, requestedAt, now time.Time) []window {
	merged := make([]window, 0, len(stored)+len(observed))

	for _, w := range stored {
		if w.reset > now.Unix() {
			merged = append(merged, w)
		}
	}

	for _, next := range observed {
		next.observedAt = now

		i := slices.IndexFunc(merged, func(w window) bool { return w.minutes == next.minutes && w.model == next.model })

		// A window reported allowed without its usage only shows that a stored limit no longer holds, as after a manual
		// reset. Like any lower usage, update takes it only from a response to a request sent after the limit arrived.
		if next.allowed && (i < 0 || !merged[i].limited) {
			continue
		}

		if i < 0 {
			merged = append(merged, next.window)
			continue
		}

		merged[i] = merged[i].update(next, requestedAt)
	}

	slices.SortFunc(merged, func(a, b window) int {
		return cmp.Or(cmp.Compare(a.minutes, b.minutes), cmp.Compare(a.model, b.model))
	})

	return merged
}

// Responses can arrive out of order. Only a response to a request sent after the stored usage
// arrived can lower the usage, as after a manual reset, or move the reset much earlier. A request
// sent before such a drop can still report the old usage, so its response is ignored.
func (w window) update(next observation, requestedAt time.Time) window {
	if requestedAt.Before(w.since) {
		return w
	}

	tolerance := int64(resetTolerance / time.Second)
	current := requestedAt.After(w.observedAt)

	switch {
	case next.earliest > w.reset+tolerance:
		return next.window
	case next.reset == 0 && w.model != "" && !requestedAt.IsZero() && requestedAt.Unix() < w.reset-w.minutes*60:
		// A model's own window resets at a fixed time, so a response without a reset whose request went out before
		// this window began belongs to the window before.
		return w
	case next.reset == 0:
		next.reset = w.reset
	case next.reset >= w.reset-tolerance:
		next.reset = min(next.reset, w.reset)
	case !current:
		return w
	}

	next.since = w.since

	switch {
	case !current:
		// An older response is a newer reading only when it shows more usage or a limit, as a late rejection does.
		// Otherwise the stored time stays, so a check already in flight can still lower the usage.
		if next.used <= w.used && (!next.limited || w.limited) {
			next.observedAt = w.observedAt
		}

		next.used = max(next.used, w.used)
		next.limited = next.limited || w.limited
	case next.used < w.used || w.limited && !next.limited:
		next.since = requestedAt
	}

	return next.window
}

func (a account) windowReports(now time.Time) []windowReport {
	reports := make([]windowReport, 0, len(a.windows))

	for _, w := range a.windows {
		report := windowReport{Minutes: w.minutes, Model: w.model, UsedPercent: w.used, Limited: w.limited}

		switch {
		case w.reset > now.Unix():
			resetsAt := time.Unix(w.reset, 0).UTC()
			report.ResetsAt = &resetsAt
		case w.reset > 0:
			report.UsedPercent, report.Limited = 0, false
		}

		reports = append(reports, report)
	}

	return reports
}

func pool(candidates []pluginapi.SchedulerAuthCandidate) (string, bool) {
	if len(candidates) == 0 {
		return "", false
	}

	provider := normalizeProvider(candidates[0].Provider)
	if _, ok := quotaReaders[provider]; !ok {
		return "", false
	}

	subscriptions := false

	for _, candidate := range candidates {
		if normalizeProvider(candidate.Provider) != provider {
			return "", false
		}

		subscriptions = subscriptions || isSubscription(candidate)
	}

	return provider, subscriptions
}

func normalizeProvider(provider string) string {
	return strings.ToLower(strings.TrimSpace(provider))
}

func isSubscription(candidate pluginapi.SchedulerAuthCandidate) bool {
	return strings.EqualFold(strings.TrimSpace(candidate.Attributes["auth_kind"]), "oauth")
}

func (r *router) less(left, right pluginapi.SchedulerAuthCandidate, now time.Time, scope string, checks bool) bool {
	leftGroup, leftReset := r.rank(left, now, scope, checks)
	rightGroup, rightReset := r.rank(right, now, scope, checks)

	if leftGroup != rightGroup {
		return leftGroup < rightGroup
	}

	if leftReset != rightReset {
		return leftReset < rightReset
	}

	return left.ID < right.ID
}

// rank places an account for a request in the scope. Only a request that reports usage can check a limit, so for a token
// count, which doesn't, an account due for a check still ranks as held.
func (r *router) rank(candidate pluginapi.SchedulerAuthCandidate, now time.Time, scope string, checks bool) (int, int64) {
	state := r.accounts[candidate.ID]
	limitedUntil, checkedAt := state.limit(now, scope)

	switch {
	case !isSubscription(candidate):
		return groupOther, 0
	case limitedUntil > 0 && (!checks || now.Before(checkedAt.Add(limitRecheck))):
		return groupLimited, limitedUntil
	case limitedUntil > 0:
		return groupRecheck, limitedUntil
	case state.reset > now.Unix():
		return groupKnown, state.reset
	case state.probedAt.IsZero() || !now.Before(state.probedAt.Add(probeRetry)):
		return groupProbe, 0
	default:
		return groupPending, 0
	}
}

func claudeQuota(headers map[string][]string, _, _ time.Time) quota {
	observed := quota{reset: headerTime(headers, "Anthropic-Ratelimit-Unified-7d-Reset")}

	// Both weekly windows reset together, so the Fable reset gives the account's order when the weekly one is missing.
	if observed.reset == 0 {
		observed.reset = headerTime(headers, "Anthropic-Ratelimit-Unified-7d_oi-Reset")
	}

	for _, claim := range claudeWindows {
		prefix := "Anthropic-Ratelimit-Unified-" + claim.name + "-"

		utilization, measured := headerFloat(headers, prefix+"Utilization")
		status := strings.ToLower(strings.TrimSpace(headerValue(headers, prefix+"Status")))
		rejected := status == "rejected"
		allowed := !measured && status == "allowed"

		if !measured && !rejected && !allowed {
			continue
		}

		used := utilization * 100
		if rejected {
			used = max(used, 100)
		}

		// Claude's two weekly windows reset together, so one that came without its own reset takes the other's.
		reset := headerTime(headers, prefix+"Reset")
		if reset == 0 && claim.minutes == 7*24*60 {
			reset = observed.reset
		}

		next := newWindow(claim.minutes, used, reset)
		next.model = claim.model
		observed.windows = append(observed.windows, observation{window: next, earliest: reset, allowed: allowed})
	}

	return observed
}

func codexQuota(headers map[string][]string, requestedAt, now time.Time) quota {
	var observed quota

	if requestedAt.IsZero() {
		requestedAt = now
	}

	longest := int64(0)

	for _, prefix := range []string{"X-Codex-Primary-", "X-Codex-Secondary-"} {
		minutes, ok := headerInt(headers, prefix+"Window-Minutes")
		if !ok || minutes <= 0 {
			continue
		}

		// Counting from when the request was sent keeps a long stream from pushing the order's reset later.
		earliest, reset := codexWindowReset(headers, prefix, requestedAt, now)
		if minutes > longest {
			longest, observed.reset = minutes, earliest
		}

		if used, ok := headerFloat(headers, prefix+"Used-Percent"); ok {
			observed.windows = append(observed.windows, observation{window: newWindow(minutes, used, reset), earliest: earliest})
		}
	}

	return observed
}

func codexWindowReset(headers map[string][]string, prefix string, requestedAt, now time.Time) (int64, int64) {
	if at := headerTime(headers, prefix+"Reset-At"); at > 0 {
		return at, at
	}

	if after, ok := headerInt(headers, prefix+"Reset-After-Seconds"); ok && after >= 0 {
		return requestedAt.Unix() + after, now.Unix() + after
	}

	return 0, 0
}

func newWindow(minutes int64, used float64, reset int64) window {
	return window{minutes: minutes, used: math.Round(min(used, 100)*10) / 10, reset: reset, limited: used >= 100}
}

func headerFloat(headers map[string][]string, name string) (float64, bool) {
	value, err := strconv.ParseFloat(strings.TrimSpace(headerValue(headers, name)), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, false
	}

	return value, true
}

func headerTime(headers map[string][]string, name string) int64 {
	raw := strings.TrimSpace(headerValue(headers, name))

	if seconds, err := strconv.ParseFloat(raw, 64); err == nil {
		if math.IsNaN(seconds) || seconds <= 0 || seconds >= math.MaxInt64 {
			return 0
		}

		return int64(seconds)
	}

	if at, err := time.Parse(time.RFC3339, raw); err == nil {
		return at.Unix()
	}

	if at, err := http.ParseTime(raw); err == nil {
		return at.Unix()
	}

	return 0
}

func headerInt(headers map[string][]string, name string) (int64, bool) {
	value, err := strconv.ParseInt(strings.TrimSpace(headerValue(headers, name)), 10, 64)
	return value, err == nil
}

func headerValue(headers map[string][]string, name string) string {
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}

	return ""
}
