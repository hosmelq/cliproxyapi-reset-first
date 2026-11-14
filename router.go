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

const version = "1.2.0"

const (
	pluginID       = "reset-first"
	probeRetry     = 15 * time.Minute
	resetTolerance = 10 * time.Minute
)

const (
	groupProbe = iota
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

var resetLogger = log.New(os.Stderr, pluginID+": ", 0)

type quota struct {
	reset   int64
	windows []observation
}

type window struct {
	minutes int64
	model   string
	used    float64
	reset   int64
	limited bool
}

type observation struct {
	window
	earliest int64
}

type account struct {
	provider     string
	priority     int
	subscription bool
	seen         bool
	reset        int64
	probedAt     time.Time
	windows      []window
}

type router struct {
	mu       sync.Mutex
	now      func() time.Time
	accounts map[string]account
	offered  map[string]map[string]bool
	serving  map[string]string
}

type statusReport struct {
	Version   string           `json:"version"`
	Now       time.Time        `json:"now"`
	Providers []providerReport `json:"providers"`
}

type providerReport struct {
	Provider string          `json:"provider"`
	Serving  string          `json:"serving,omitempty"`
	Accounts []accountReport `json:"accounts"`
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
		offered:  make(map[string]map[string]bool),
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

	now := r.now()
	state := r.accounts[record.AuthID]
	state.provider = provider

	requestedAt := record.RequestedAt
	if requestedAt.IsZero() || requestedAt.After(now) {
		requestedAt = now
	}

	observed := readQuota(record.ResponseHeaders, requestedAt, now)

	wasLimited := state.limitedUntil(now) > 0
	state.windows = mergeWindows(state.windows, observed.windows, now)

	if until := state.limitedUntil(now); until > 0 && !wasLimited {
		resetLogger.Printf("auth %q reached a quota limit until %s", record.AuthID, time.Unix(until, 0).UTC().Format(time.RFC3339))
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

	r.offered[provider] = offered

	if !subscriptions {
		return pluginapi.SchedulerPickResponse{}
	}

	now := r.now()

	chosen := req.Candidates[0]
	for _, candidate := range req.Candidates[1:] {
		if r.less(candidate, chosen, now) {
			chosen = candidate
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
		offered := r.offered[provider]

		sort.Slice(candidates, func(i, j int) bool {
			left, right := candidates[i], candidates[j]

			if offered[left.ID] != offered[right.ID] {
				return offered[left.ID]
			}

			if left.Priority != right.Priority {
				return left.Priority > right.Priority
			}

			return r.less(left, right, now)
		})

		pool := providerReport{Provider: provider, Serving: r.serving[provider], Accounts: make([]accountReport, 0, len(candidates))}
		for _, candidate := range candidates {
			pool.Accounts = append(pool.Accounts, r.accountReport(candidate, offered[candidate.ID], now))
		}

		report.Providers = append(report.Providers, pool)
	}

	return report
}

func (r *router) accountReport(candidate pluginapi.SchedulerAuthCandidate, available bool, now time.Time) accountReport {
	state := r.accounts[candidate.ID]
	report := accountReport{ID: candidate.ID, Available: available, Priority: state.priority, Windows: state.windowReports(now)}

	if state.reset > now.Unix() {
		resetsAt := time.Unix(state.reset, 0).UTC()
		report.ResetsAt = &resetsAt
	}

	switch group, until := r.rank(candidate, now); group {
	case groupLimited:
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

func (a account) limitedUntil(now time.Time) int64 {
	until := int64(0)

	// A model's own limit, such as Fable's weekly limit, only stops requests for that model.
	for _, w := range a.windows {
		if w.model == "" && w.limited && w.reset > now.Unix() && w.reset > until {
			until = w.reset
		}
	}

	return until
}

func mergeWindows(stored []window, observed []observation, now time.Time) []window {
	merged := make([]window, 0, len(stored)+len(observed))

	for _, w := range stored {
		if w.reset > now.Unix() {
			merged = append(merged, w)
		}
	}

	for _, next := range observed {
		i := slices.IndexFunc(merged, func(w window) bool { return w.minutes == next.minutes && w.model == next.model })
		if i < 0 {
			merged = append(merged, next.window)
			continue
		}

		merged[i] = merged[i].update(next)
	}

	slices.SortFunc(merged, func(a, b window) int {
		return cmp.Or(cmp.Compare(a.minutes, b.minutes), cmp.Compare(a.model, b.model))
	})

	return merged
}

func (w window) update(next observation) window {
	tolerance := int64(resetTolerance / time.Second)

	switch {
	case next.reset == 0:
		next.reset = w.reset
	case next.reset < w.reset-tolerance:
		return w
	case next.earliest > w.reset+tolerance:
		return next.window
	default:
		next.reset = min(next.reset, w.reset)
	}

	next.used = max(next.used, w.used)
	next.limited = next.limited || w.limited

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

func (r *router) less(left, right pluginapi.SchedulerAuthCandidate, now time.Time) bool {
	leftGroup, leftReset := r.rank(left, now)
	rightGroup, rightReset := r.rank(right, now)

	if leftGroup != rightGroup {
		return leftGroup < rightGroup
	}

	if leftReset != rightReset {
		return leftReset < rightReset
	}

	return left.ID < right.ID
}

func (r *router) rank(candidate pluginapi.SchedulerAuthCandidate, now time.Time) (int, int64) {
	state := r.accounts[candidate.ID]
	limitedUntil := state.limitedUntil(now)

	switch {
	case !isSubscription(candidate):
		return groupOther, 0
	case limitedUntil > 0:
		return groupLimited, limitedUntil
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

	for _, claim := range claudeWindows {
		prefix := "Anthropic-Ratelimit-Unified-" + claim.name + "-"

		utilization, measured := headerFloat(headers, prefix+"Utilization")
		rejected := strings.EqualFold(strings.TrimSpace(headerValue(headers, prefix+"Status")), "rejected")

		if !measured && !rejected {
			continue
		}

		used := utilization * 100
		if rejected {
			used = max(used, 100)
		}

		reset := headerTime(headers, prefix+"Reset")
		next := newWindow(claim.minutes, used, reset)
		next.model = claim.model
		observed.windows = append(observed.windows, observation{window: next, earliest: reset})
	}

	return observed
}

func codexQuota(headers map[string][]string, requestedAt, now time.Time) quota {
	var observed quota

	longest := int64(0)

	for _, prefix := range []string{"X-Codex-Primary-", "X-Codex-Secondary-"} {
		minutes, ok := headerInt(headers, prefix+"Window-Minutes")
		if !ok || minutes <= 0 {
			continue
		}

		earliest, reset := codexWindowReset(headers, prefix, requestedAt, now)
		if minutes > longest {
			longest, observed.reset = minutes, reset
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
