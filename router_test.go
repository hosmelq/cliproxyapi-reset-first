package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func testRouter() (*router, *time.Time) {
	now := time.Unix(2_000_000_000, 0).UTC()
	return newRouter(func() time.Time { return now }), &now
}

func credential(provider, kind, id string) pluginapi.SchedulerAuthCandidate {
	return pluginapi.SchedulerAuthCandidate{ID: id, Provider: provider, Attributes: map[string]string{"auth_kind": kind}}
}

func subscription(id string) pluginapi.SchedulerAuthCandidate {
	return credential("claude", "oauth", id)
}

func apiKey(id string) pluginapi.SchedulerAuthCandidate {
	return credential("claude", "apikey", id)
}

func request(candidates ...pluginapi.SchedulerAuthCandidate) pluginapi.SchedulerPickRequest {
	return pluginapi.SchedulerPickRequest{Candidates: candidates}
}

func pickRequest(ids ...string) pluginapi.SchedulerPickRequest {
	req := request()
	for _, id := range ids {
		req.Candidates = append(req.Candidates, subscription(id))
	}

	return req
}

func codexRequest(ids ...string) pluginapi.SchedulerPickRequest {
	req := request()
	for _, id := range ids {
		req.Candidates = append(req.Candidates, credential("codex", "oauth", id))
	}

	return req
}

func unix(t time.Time) string {
	return strconv.FormatInt(t.Unix(), 10)
}

func observeReset(r *router, id string, reset time.Time) {
	r.Observe(pluginapi.UsageRecord{
		Provider:        "claude",
		AuthID:          id,
		ResponseHeaders: http.Header{"Anthropic-Ratelimit-Unified-7d-Reset": {unix(reset)}},
	})
}

func observeClaude(r *router, id string, headers http.Header) {
	r.Observe(pluginapi.UsageRecord{Provider: "claude", AuthID: id, ResponseHeaders: headers})
}

func observeCodex(r *router, id string, headers http.Header) {
	r.Observe(pluginapi.UsageRecord{Provider: "codex", AuthID: id, ResponseHeaders: headers})
}

func wantPick(t *testing.T, r *router, req pluginapi.SchedulerPickRequest, want string) {
	t.Helper()

	got := r.Pick(req)
	if !got.Handled || got.AuthID != want {
		t.Fatalf("Pick() = %+v, want handled auth %q", got, want)
	}
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()

	var output bytes.Buffer

	previous := resetLogger.Writer()
	resetLogger.SetOutput(&output)
	t.Cleanup(func() { resetLogger.SetOutput(previous) })

	return &output
}

func TestLearningAndSoonestReset(t *testing.T) {
	r, now := testRouter()

	observeReset(r, "a", now.Add(6*24*time.Hour))
	r.Observe(pluginapi.UsageRecord{
		Provider: "CLAUDE",
		AuthID:   "b",
		Failed:   true,
		ResponseHeaders: http.Header{
			"aNtHrOpIc-RaTeLiMiT-uNiFiEd-7d-ReSeT": {" " + unix(now.Add(24*time.Hour)) + " "},
		},
	})
	observeReset(r, "c", now.Add(6*24*time.Hour+time.Hour))

	wantPick(t, r, pickRequest("c", "a", "b"), "b")

	observeReset(r, "a", now.Add(time.Hour))

	wantPick(t, r, pickRequest("c", "b", "a"), "a")
}

func TestEqualResetsUseAuthID(t *testing.T) {
	r, now := testRouter()

	for _, id := range []string{"c", "a", "b"} {
		observeReset(r, id, now.Add(24*time.Hour))
	}

	wantPick(t, r, pickRequest("c", "b", "a"), "a")
}

func TestRoutingMovesOnlyWhenTheBestAccountChanges(t *testing.T) {
	r, now := testRouter()

	observeReset(r, "a", now.Add(6*24*time.Hour))
	observeReset(r, "b", now.Add(24*time.Hour))
	observeReset(r, "c", now.Add(6*24*time.Hour+time.Hour))

	all := pickRequest("a", "b", "c")
	withoutB := pickRequest("a", "c")

	wantPick(t, r, all, "b")
	wantPick(t, r, all, "b")
	wantPick(t, r, withoutB, "a")
	wantPick(t, r, all, "b")
}

func TestUnknownSubscriptionsAreProbedFirst(t *testing.T) {
	r, now := testRouter()

	observeReset(r, "b", now.Add(24*time.Hour))
	observeReset(r, "c", now.Add(6*24*time.Hour))

	req := pickRequest("d", "c", "b", "a")
	wantPick(t, r, req, "a")

	observeReset(r, "a", now.Add(3*24*time.Hour))
	wantPick(t, r, req, "d")

	observeReset(r, "d", now.Add(2*24*time.Hour))
	wantPick(t, r, req, "b")
}

func TestRoutingFollowsResetsAsTheyAreLearned(t *testing.T) {
	r, now := testRouter()

	all := pickRequest("a", "b", "c")
	wantPick(t, r, all, "a")

	observeReset(r, "a", now.Add(6*24*time.Hour))
	wantPick(t, r, all, "b")

	observeReset(r, "b", now.Add(24*time.Hour))
	wantPick(t, r, all, "c")

	observeReset(r, "c", now.Add(6*24*time.Hour+time.Hour))
	wantPick(t, r, all, "b")
}

func TestPicksWithoutUsageDoNotUseUpProbes(t *testing.T) {
	r, now := testRouter()

	observeReset(r, "b", now.Add(6*24*time.Hour))

	req := pickRequest("a", "b")
	wantPick(t, r, req, "a")
	wantPick(t, r, req, "a")

	r.Observe(pluginapi.UsageRecord{Provider: "claude", AuthID: "a", Failed: true})
	wantPick(t, r, req, "b")
}

func TestFailedProbeIsRetriedAfterInterval(t *testing.T) {
	r, now := testRouter()

	req := pickRequest("c", "b", "a")
	for _, id := range []string{"a", "b", "c"} {
		wantPick(t, r, req, id)
		r.Observe(pluginapi.UsageRecord{Provider: "claude", AuthID: id, Failed: true})
	}

	wantPick(t, r, req, "a")

	*now = now.Add(probeRetry)

	wantPick(t, r, req, "a")
	observeReset(r, "a", now.Add(6*24*time.Hour))
	wantPick(t, r, req, "b")
}

func TestPassedResetIsReprobed(t *testing.T) {
	r, now := testRouter()

	firstReset := now.Add(time.Hour)
	observeReset(r, "a", firstReset)
	observeReset(r, "b", now.Add(6*24*time.Hour))

	req := pickRequest("b", "a")
	wantPick(t, r, req, "a")

	*now = firstReset
	wantPick(t, r, req, "a")

	observeReset(r, "a", firstReset)
	wantPick(t, r, req, "b")

	*now = now.Add(probeRetry)
	wantPick(t, r, req, "a")

	observeReset(r, "a", firstReset.Add(7*24*time.Hour))
	wantPick(t, r, req, "b")
}

func TestCodexUsesItsLongestWindow(t *testing.T) {
	r, now := testRouter()

	observeCodex(r, "a", http.Header{
		"X-Codex-Primary-Window-Minutes":   {"300"},
		"X-Codex-Primary-Reset-At":         {unix(now.Add(time.Hour))},
		"X-Codex-Secondary-Window-Minutes": {"10080"},
		"X-Codex-Secondary-Reset-At":       {unix(now.Add(6 * 24 * time.Hour))},
	})
	observeCodex(r, "b", http.Header{
		"X-Codex-Primary-Window-Minutes": {"10080"},
		"X-Codex-Primary-Reset-At":       {unix(now.Add(2 * 24 * time.Hour))},
	})

	wantPick(t, r, codexRequest("a", "b"), "b")
}

func TestCodexResetAfterSecondsIsLearnedOnce(t *testing.T) {
	output := captureLogs(t)
	r, now := testRouter()

	observeCodex(r, "b", http.Header{
		"X-Codex-Secondary-Window-Minutes": {"10080"},
		"X-Codex-Secondary-Reset-At":       {unix(now.Add(2 * 24 * time.Hour))},
	})
	observeCodex(r, "a", http.Header{
		"X-Codex-Secondary-Window-Minutes":      {"10080"},
		"X-Codex-Secondary-Reset-After-Seconds": {"86400"},
	})

	*now = now.Add(3 * time.Minute)

	observeCodex(r, "a", http.Header{
		"X-Codex-Secondary-Window-Minutes":      {"10080"},
		"X-Codex-Secondary-Reset-After-Seconds": {strconv.Itoa(86400 - 150)},
	})

	wantPick(t, r, codexRequest("a", "b"), "a")

	if lines := strings.Count(output.String(), "\n"); lines != 2 {
		t.Fatalf("logs = %q, want one reset per account", output.String())
	}
}

func TestUsedUpAccountWaitsForItsWindowToReset(t *testing.T) {
	r, now := testRouter()

	observeReset(r, "a", now.Add(24*time.Hour))
	observeReset(r, "b", now.Add(6*24*time.Hour))

	req := request(subscription("a"), subscription("b"), apiKey("k"))
	wantPick(t, r, req, "a")

	fiveHourReset := now.Add(2 * time.Hour)
	r.Observe(pluginapi.UsageRecord{
		Provider: "claude",
		AuthID:   "a",
		Failed:   true,
		ResponseHeaders: http.Header{
			"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"},
			"Anthropic-Ratelimit-Unified-5h-Reset":  {unix(fiveHourReset)},
			"Anthropic-Ratelimit-Unified-7d-Reset":  {unix(now.Add(24 * time.Hour))},
		},
	})

	wantPick(t, r, req, "b")
	wantPick(t, r, request(subscription("a"), apiKey("k")), "k")

	*now = fiveHourReset
	wantPick(t, r, req, "a")
}

func TestFullUtilizationCountsAsUsedUp(t *testing.T) {
	r, now := testRouter()

	weeklyReset := now.Add(24 * time.Hour)
	observeReset(r, "b", now.Add(6*24*time.Hour))
	observeClaude(r, "a", http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.2"},
		"Anthropic-Ratelimit-Unified-5h-Reset":       {unix(now.Add(time.Hour))},
		"Anthropic-Ratelimit-Unified-7d-Utilization": {"1.0"},
		"Anthropic-Ratelimit-Unified-7d-Reset":       {unix(weeklyReset)},
	})

	req := pickRequest("a", "b")
	wantPick(t, r, req, "b")

	*now = weeklyReset
	wantPick(t, r, req, "a")
}

func TestPartialUsageKeepsAnActiveLimit(t *testing.T) {
	r, now := testRouter()

	observeReset(r, "b", now.Add(6*24*time.Hour))
	observeClaude(r, "a", http.Header{
		"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"},
		"Anthropic-Ratelimit-Unified-5h-Reset":  {unix(now.Add(time.Hour))},
		"Anthropic-Ratelimit-Unified-7d-Reset":  {unix(now.Add(24 * time.Hour))},
	})
	observeClaude(r, "a", http.Header{
		"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.5"},
		"Anthropic-Ratelimit-Unified-7d-Reset":       {unix(now.Add(24 * time.Hour))},
	})

	req := pickRequest("a", "b")
	wantPick(t, r, req, "b")

	*now = now.Add(time.Hour)
	wantPick(t, r, req, "a")
}

func TestLateOrPartialUsageKeepsALimitUntilANewWindow(t *testing.T) {
	r, now := testRouter()

	fiveHourReset := now.Add(time.Hour)
	observeReset(r, "a", now.Add(24*time.Hour))
	observeReset(r, "b", now.Add(6*24*time.Hour))

	req := pickRequest("a", "b")

	for _, headers := range []http.Header{
		{"Anthropic-Ratelimit-Unified-5h-Utilization": {"1"}, "Anthropic-Ratelimit-Unified-5h-Reset": {unix(fiveHourReset)}},
		{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.9"}, "Anthropic-Ratelimit-Unified-5h-Reset": {unix(fiveHourReset)}},
		{"Anthropic-Ratelimit-Unified-5h-Utilization": {"1"}},
	} {
		observeClaude(r, "a", headers)
		wantPick(t, r, req, "b")
	}

	observeClaude(r, "a", http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.05"},
		"Anthropic-Ratelimit-Unified-5h-Reset":       {unix(fiveHourReset.Add(4 * time.Hour))},
	})

	wantPick(t, r, req, "a")
}

func TestResetsInOtherTimestampFormats(t *testing.T) {
	r, now := testRouter()

	r.Pick(pickRequest("a", "c"))

	fiveHourReset := now.Add(time.Hour)
	weeklyReset := now.Add(24 * time.Hour)
	otherReset := now.Add(48 * time.Hour)

	observeClaude(r, "a", http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization": {"1"},
		"Anthropic-Ratelimit-Unified-5h-Reset":       {fiveHourReset.Format(time.RFC3339)},
		"Anthropic-Ratelimit-Unified-7d-Reset":       {unix(weeklyReset) + ".5"},
	})
	observeClaude(r, "c", http.Header{
		"Anthropic-Ratelimit-Unified-7d-Reset": {otherReset.Format(http.TimeFormat)},
	})

	pool := r.Status().Providers[0]
	wantAccounts(t, pool, "c:known", "a:limited")

	c, a := pool.Accounts[0], pool.Accounts[1]
	if a.LimitedUntil == nil || !a.LimitedUntil.Equal(fiveHourReset) || a.ResetsAt == nil || !a.ResetsAt.Equal(weeklyReset) {
		t.Fatalf("a = %+v", a)
	}

	if c.ResetsAt == nil || !c.ResetsAt.Equal(otherReset) {
		t.Fatalf("c = %+v", c)
	}
}

func TestCodexUsedUpWindowWaitsForItsReset(t *testing.T) {
	r, now := testRouter()

	observeCodex(r, "a", http.Header{
		"X-Codex-Primary-Window-Minutes":      {"300"},
		"X-Codex-Primary-Used-Percent":        {"100"},
		"X-Codex-Primary-Reset-After-Seconds": {"3600"},
		"X-Codex-Secondary-Window-Minutes":    {"10080"},
		"X-Codex-Secondary-Used-Percent":      {"40.5"},
		"X-Codex-Secondary-Reset-At":          {unix(now.Add(24 * time.Hour))},
	})
	observeCodex(r, "b", http.Header{
		"X-Codex-Secondary-Window-Minutes": {"10080"},
		"X-Codex-Secondary-Reset-At":       {unix(now.Add(6 * 24 * time.Hour))},
	})

	req := codexRequest("a", "b")
	wantPick(t, r, req, "b")

	*now = now.Add(time.Hour)
	wantPick(t, r, req, "a")
}

func TestCodexLateRateLimitEventHoldsTheAccount(t *testing.T) {
	r, now := testRouter()

	observeCodex(r, "b", http.Header{
		"X-Codex-Secondary-Window-Minutes": {"10080"},
		"X-Codex-Secondary-Reset-At":       {unix(now.Add(6 * 24 * time.Hour))},
	})
	observeCodex(r, "a", http.Header{
		"X-Codex-Secondary-Window-Minutes": {"10080"},
		"X-Codex-Secondary-Reset-At":       {unix(now.Add(24 * time.Hour))},
	})

	requestedAt := *now
	*now = now.Add(13 * time.Minute)

	r.Observe(pluginapi.UsageRecord{
		Provider:    "codex",
		AuthID:      "a",
		RequestedAt: requestedAt,
		ResponseHeaders: http.Header{
			"X-Codex-Primary-Window-Minutes":      {"300"},
			"X-Codex-Primary-Used-Percent":        {"100"},
			"X-Codex-Primary-Reset-After-Seconds": {"480"},
		},
	})

	wantPick(t, r, codexRequest("a", "b"), "b")
}

func TestCodexLateStreamKeepsALimit(t *testing.T) {
	r, now := testRouter()

	observeCodex(r, "b", http.Header{
		"X-Codex-Secondary-Window-Minutes": {"10080"},
		"X-Codex-Secondary-Reset-At":       {unix(now.Add(6 * 24 * time.Hour))},
	})
	observeCodex(r, "a", http.Header{
		"X-Codex-Primary-Window-Minutes":   {"300"},
		"X-Codex-Primary-Used-Percent":     {"100"},
		"X-Codex-Primary-Reset-At":         {unix(now.Add(time.Hour))},
		"X-Codex-Secondary-Window-Minutes": {"10080"},
		"X-Codex-Secondary-Reset-At":       {unix(now.Add(24 * time.Hour))},
	})

	requestedAt := *now
	*now = now.Add(12 * time.Minute)

	r.Observe(pluginapi.UsageRecord{
		Provider:    "codex",
		AuthID:      "a",
		RequestedAt: requestedAt,
		ResponseHeaders: http.Header{
			"X-Codex-Primary-Window-Minutes":      {"300"},
			"X-Codex-Primary-Used-Percent":        {"90"},
			"X-Codex-Primary-Reset-After-Seconds": {"3600"},
		},
	})

	req := codexRequest("a", "b")
	wantPick(t, r, req, "b")

	*now = requestedAt.Add(time.Hour)
	wantPick(t, r, req, "a")
}

func TestMalformedUtilizationIsIgnored(t *testing.T) {
	r, now := testRouter()

	wantPick(t, r, pickRequest("a"), "a")
	wantPick(t, r, codexRequest("c"), "c")

	for _, value := range []string{"", "busy", "NaN", "Inf", "-0.5"} {
		observeClaude(r, "a", http.Header{
			"Anthropic-Ratelimit-Unified-5h-Utilization": {value},
			"Anthropic-Ratelimit-Unified-5h-Reset":       {unix(now.Add(time.Hour))},
		})
		observeCodex(r, "c", http.Header{
			"X-Codex-Primary-Window-Minutes": {"300"},
			"X-Codex-Primary-Used-Percent":   {value},
			"X-Codex-Primary-Reset-At":       {unix(now.Add(time.Hour))},
		})
	}

	for _, pool := range r.Status().Providers {
		if windows := pool.Accounts[0].Windows; len(windows) != 0 {
			t.Fatalf("%s windows = %+v, want none", pool.Provider, windows)
		}
	}
}

func TestLogsWhenAnAccountReachesALimit(t *testing.T) {
	output := captureLogs(t)
	r, now := testRouter()

	headers := http.Header{
		"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"},
		"Anthropic-Ratelimit-Unified-5h-Reset":  {unix(now.Add(time.Hour))},
	}

	observeClaude(r, "a", headers)
	observeClaude(r, "a", headers)

	if got := strings.Count(output.String(), `auth "a" reached a quota limit until `+now.Add(time.Hour).Format(time.RFC3339)); got != 1 {
		t.Fatalf("logs = %q, want one limit", output.String())
	}
}

func TestAPIKeysRankAfterSubscriptions(t *testing.T) {
	r, now := testRouter()

	req := request(apiKey("a"), subscription("b"))
	wantPick(t, r, req, "b")

	r.Observe(pluginapi.UsageRecord{Provider: "claude", AuthID: "b", Failed: true})
	wantPick(t, r, req, "b")

	observeReset(r, "a", now.Add(24*time.Hour))
	observeReset(r, "b", now.Add(6*24*time.Hour))
	wantPick(t, r, req, "b")
}

func TestPicksOutsideSupportedSubscriptionPoolsAreUnhandled(t *testing.T) {
	r, _ := testRouter()

	unclassified := subscription("a")
	unclassified.Attributes = nil

	for _, req := range []pluginapi.SchedulerPickRequest{
		request(),
		request(credential("gemini-cli", "oauth", "a")),
		request(subscription("a"), credential("codex", "oauth", "b")),
		request(subscription("a"), credential("openai", "apikey", "b")),
		request(apiKey("a"), apiKey("b")),
		request(credential("codex", "apikey", "a")),
		request(unclassified),
	} {
		if got := r.Pick(req); got.Handled {
			t.Fatalf("Pick(%+v) = %+v, want unhandled", req, got)
		}
	}
}

func TestMalformedAndUnrelatedUsageIsIgnored(t *testing.T) {
	r, now := testRouter()

	observeReset(r, "a", now.Add(24*time.Hour))
	observeReset(r, "b", now.Add(6*24*time.Hour))

	past := unix(now.Add(-time.Hour))
	soon := unix(now.Add(time.Minute))

	for _, value := range []string{"", "no reset", "0", "-1", "1.5", "99999999999999999999999", past} {
		headers := http.Header{"Anthropic-Ratelimit-Unified-7d-Reset": {value}}
		r.Observe(pluginapi.UsageRecord{Provider: "claude", AuthID: "b", ResponseHeaders: headers})
	}

	for _, record := range []pluginapi.UsageRecord{
		{},
		{Provider: "claude", AuthID: "b", ResponseHeaders: http.Header{"Anthropic-Ratelimit-Unified-7d-Reset": {}}},
		{Provider: "claude", AuthID: "", ResponseHeaders: http.Header{"Anthropic-Ratelimit-Unified-7d-Reset": {soon}}},
		{Provider: "openai", AuthID: "b", ResponseHeaders: http.Header{"Anthropic-Ratelimit-Unified-7d-Reset": {soon}}},
		{Provider: "codex", AuthID: "b", ResponseHeaders: http.Header{"Anthropic-Ratelimit-Unified-7d-Reset": {soon}}},
		{Provider: "claude", AuthID: "b", ResponseHeaders: http.Header{
			"X-Codex-Primary-Window-Minutes": {"10080"},
			"X-Codex-Primary-Reset-At":       {soon},
		}},
	} {
		r.Observe(record)
	}

	wantPick(t, r, pickRequest("a", "b"), "a")
}

func TestMalformedCodexUsageIsIgnored(t *testing.T) {
	r, now := testRouter()

	observeCodex(r, "a", http.Header{
		"X-Codex-Secondary-Window-Minutes": {"10080"},
		"X-Codex-Secondary-Reset-At":       {unix(now.Add(24 * time.Hour))},
	})
	observeCodex(r, "b", http.Header{
		"X-Codex-Secondary-Window-Minutes": {"10080"},
		"X-Codex-Secondary-Reset-At":       {unix(now.Add(6 * 24 * time.Hour))},
	})

	soon := unix(now.Add(time.Minute))

	for _, headers := range []http.Header{
		{"X-Codex-Secondary-Reset-At": {soon}},
		{"X-Codex-Secondary-Window-Minutes": {"0"}, "X-Codex-Secondary-Reset-At": {soon}},
		{"X-Codex-Secondary-Window-Minutes": {"week"}, "X-Codex-Secondary-Reset-At": {soon}},
		{"X-Codex-Secondary-Window-Minutes": {"10080"}},
		{"X-Codex-Secondary-Window-Minutes": {"10080"}, "X-Codex-Secondary-Reset-At": {"0"}, "X-Codex-Secondary-Reset-After-Seconds": {"-5"}},
		{
			"X-Codex-Primary-Window-Minutes":   {"300"},
			"X-Codex-Primary-Reset-At":         {soon},
			"X-Codex-Secondary-Window-Minutes": {"10080"},
		},
	} {
		observeCodex(r, "b", headers)
	}

	wantPick(t, r, codexRequest("a", "b"), "a")
}

func TestLogsOnlyChangedResets(t *testing.T) {
	output := captureLogs(t)
	r, now := testRouter()
	reset := now.Add(24 * time.Hour)

	observeReset(r, "a", reset)
	observeReset(r, "a", reset)
	r.Observe(pluginapi.UsageRecord{})
	wantPick(t, r, pickRequest("a"), "a")
	observeReset(r, "a", reset.Add(time.Hour))

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("logs = %q, want two changed resets", output.String())
	}

	for i, line := range lines {
		wantReset := reset.Add(time.Duration(i) * time.Hour).Format(time.RFC3339)
		if !strings.Contains(line, `"a"`) || !strings.Contains(line, wantReset) {
			t.Fatalf("log = %q, want auth a and reset %s", line, wantReset)
		}
	}
}

func TestConcurrentPicksAndObservations(t *testing.T) {
	r, now := testRouter()

	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for range 100 {
				observeReset(r, "a", now.Add(24*time.Hour))
				observeReset(r, "b", now.Add(6*24*time.Hour))

				got := r.Pick(pickRequest("a", "b"))
				if !got.Handled || (got.AuthID != "a" && got.AuthID != "b") {
					t.Errorf("concurrent Pick() = %+v", got)
				}
			}
		}()
	}

	wg.Wait()
}

func TestRegistrationAndMalformedUsageRPC(t *testing.T) {
	for _, method := range []string{pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure} {
		raw, err := handleMethod(method, []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}

		var envelope pluginabi.Envelope
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatal(err)
		}

		var got registration
		if err := json.Unmarshal(envelope.Result, &got); err != nil {
			t.Fatal(err)
		}

		if !envelope.OK ||
			!got.Capabilities.Scheduler ||
			!got.Capabilities.UsagePlugin ||
			!got.Capabilities.ManagementAPI ||
			got.SchemaVersion != pluginabi.SchemaVersion ||
			got.Metadata.Name != pluginID ||
			got.Metadata.Version != version {
			t.Fatalf("registration = %+v, envelope = %+v", got, envelope)
		}
	}

	for _, raw := range []string{"", "{", "null", `[]`, `{"Provider":"claude","AuthID":"a","ResponseHeaders":{"x":1}}`} {
		result, err := handleMethod(pluginabi.MethodUsageHandle, []byte(raw))
		if err != nil || string(result) != `{"ok":true,"result":{}}` {
			t.Fatalf("malformed usage %q: result = %s, error = %v", raw, result, err)
		}
	}
}

func TestMalformedPickIsLeftToCPA(t *testing.T) {
	for _, raw := range []string{"", "{", `{"Candidates":"a"}`} {
		result, err := handleMethod(pluginabi.MethodSchedulerPick, []byte(raw))
		if err != nil {
			t.Fatalf("malformed pick %q: error = %v", raw, err)
		}

		var envelope pluginabi.Envelope
		if err := json.Unmarshal(result, &envelope); err != nil || !envelope.OK {
			t.Fatalf("malformed pick %q: envelope = %s, error = %v", raw, result, err)
		}

		var got pluginapi.SchedulerPickResponse
		if err := json.Unmarshal(envelope.Result, &got); err != nil || got.Handled {
			t.Fatalf("malformed pick %q: response = %+v, error = %v", raw, got, err)
		}
	}
}
