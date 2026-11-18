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

func TestFableLimitIsReportedWithoutMovingTheAccount(t *testing.T) {
	r, now := testRouter()

	weeklyReset := now.Add(24 * time.Hour)
	observeReset(r, "b", now.Add(6*24*time.Hour))
	observeClaude(r, "a", http.Header{
		"Anthropic-Ratelimit-Unified-Status":            {"rejected"},
		"Anthropic-Ratelimit-Unified-7d-Status":         {"allowed"},
		"Anthropic-Ratelimit-Unified-7d-Utilization":    {"0.62"},
		"Anthropic-Ratelimit-Unified-7d-Reset":          {unix(weeklyReset)},
		"Anthropic-Ratelimit-Unified-7d_oi-Status":      {"rejected"},
		"Anthropic-Ratelimit-Unified-7d_oi-Utilization": {"1.0"},
		"Anthropic-Ratelimit-Unified-7d_oi-Reset":       {unix(weeklyReset)},
	})

	wantPick(t, r, pickRequest("a", "b"), "a")

	observeClaude(r, "a", http.Header{
		"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.63"},
		"Anthropic-Ratelimit-Unified-7d-Reset":       {unix(weeklyReset)},
	})

	account := r.Status().Providers[0].Accounts[0]
	if account.ID != "a" || account.State != "known" || len(account.Windows) != 2 {
		t.Fatalf("account = %+v", account)
	}

	weekly, fable := account.Windows[0], account.Windows[1]
	if weekly.Model != "" || weekly.UsedPercent != 63 || weekly.Limited {
		t.Fatalf("weekly window = %+v", weekly)
	}

	if fable.Model != "fable" || fable.Minutes != 10080 || fable.UsedPercent != 100 || !fable.Limited || fable.ResetsAt == nil || !fable.ResetsAt.Equal(weeklyReset) {
		t.Fatalf("Fable window = %+v", fable)
	}

	if raw, err := json.Marshal(fable); err != nil || !strings.Contains(string(raw), `"model":"fable"`) {
		t.Fatalf("Fable window JSON = %s, err = %v", raw, err)
	}
}

func modelRequest(model string, ids ...string) pluginapi.SchedulerPickRequest {
	req := pickRequest(ids...)
	req.Model = model

	return req
}

func fableUsedUp(weeklyReset, fableReset time.Time) http.Header {
	return http.Header{
		"Anthropic-Ratelimit-Unified-7d-Utilization":    {"0.81"},
		"Anthropic-Ratelimit-Unified-7d-Reset":          {unix(weeklyReset)},
		"Anthropic-Ratelimit-Unified-7d_oi-Status":      {"rejected"},
		"Anthropic-Ratelimit-Unified-7d_oi-Utilization": {"1.0"},
		"Anthropic-Ratelimit-Unified-7d_oi-Reset":       {unix(fableReset)},
	}
}

func TestFableRequestsSkipAnAccountWithoutFable(t *testing.T) {
	output := captureLogs(t)
	r, now := testRouter()

	fableReset := now.Add(2 * time.Hour)
	observeReset(r, "b", now.Add(3*24*time.Hour))
	observeClaude(r, "a", fableUsedUp(now.Add(24*time.Hour), fableReset))

	for _, model := range []string{"", "claude-opus-5-5", "claude-haiku-5-5"} {
		wantPick(t, r, modelRequest(model, "a", "b"), "a")
	}

	for _, model := range []string{"claude-fable-5-2", "Claude-FABLE-5-2"} {
		wantPick(t, r, modelRequest(model, "a", "b"), "b")
	}

	wantPick(t, r, modelRequest("claude-fable-5-2", "a"), "a")

	*now = fableReset
	wantPick(t, r, modelRequest("claude-fable-5-2", "a", "b"), "a")

	logs := output.String()
	if !strings.Contains(logs, `auth "a" reached its fable limit until `+fableReset.Format(time.RFC3339)) || strings.Contains(logs, "reached a quota limit") {
		t.Fatalf("logs = %q, want only the Fable limit", logs)
	}
}

func TestFableLimitIsCheckedAgainEveryFewMinutes(t *testing.T) {
	output := captureLogs(t)
	r, now := testRouter()

	weeklyReset := now.Add(24 * time.Hour)
	observeReset(r, "b", now.Add(3*24*time.Hour))
	observeClaude(r, "a", fableUsedUp(weeklyReset, weeklyReset))

	fable := modelRequest("claude-fable-5-2", "a", "b")
	other := modelRequest("claude-opus-5-5", "a", "b")

	*now = now.Add(4*time.Minute + 59*time.Second)
	wantPick(t, r, fable, "b")

	// A request for another model doesn't count against the Fable limit, so it isn't a check.
	*now = now.Add(time.Second)
	wantPick(t, r, other, "a")
	wantPick(t, r, fable, "a")
	wantPick(t, r, fable, "b")

	*now = now.Add(5 * time.Minute)
	wantPick(t, r, fable, "a")

	// The check finds the Fable limit reset early, for example by a manual reset.
	r.Observe(pluginapi.UsageRecord{
		Provider:    "claude",
		AuthID:      "a",
		RequestedAt: *now,
		ResponseHeaders: http.Header{
			"Anthropic-Ratelimit-Unified-7d_oi-Status":      {"allowed"},
			"Anthropic-Ratelimit-Unified-7d_oi-Utilization": {"0.0"},
			"Anthropic-Ratelimit-Unified-7d_oi-Reset":       {unix(weeklyReset)},
		},
	})

	wantPick(t, r, fable, "a")
	wantPick(t, r, fable, "a")

	if !strings.Contains(output.String(), `auth "a" is below its fable limit again`) {
		t.Fatalf("logs = %q, want the Fable limit cleared", output.String())
	}
}

func TestTokenCountDoesNotUseUpACheck(t *testing.T) {
	r, now := testRouter()

	weeklyReset := now.Add(24 * time.Hour)
	observeReset(r, "b", now.Add(3*24*time.Hour))
	observeClaude(r, "a", fableUsedUp(weeklyReset, weeklyReset))

	fable := modelRequest("claude-fable-5-2", "a", "b")
	count := modelRequest("claude-fable-5-2", "a", "b")
	count.Options.Metadata = map[string]any{"request_path": "/v1/messages/count_tokens"}

	// A token count can't check the limit, so it goes to b and leaves the check to the next Fable message.
	*now = now.Add(5 * time.Minute)
	wantPick(t, r, count, "b")
	wantPick(t, r, fable, "a")
	wantPick(t, r, fable, "b")

	// A token count retried on b alone changes neither where the page says requests go nor the offer for other models.
	wantPick(t, r, modelRequest("claude-opus-5-5", "a", "b"), "a")

	onlyB := modelRequest("claude-fable-5-2", "b")
	onlyB.Options.Metadata = map[string]any{"request_path": "/v1/messages/count_tokens"}
	wantPick(t, r, onlyB, "b")

	onlyB.Model = "claude-opus-5-5"
	wantPick(t, r, onlyB, "b")

	report := r.Status().Providers[0]
	wantAccounts(t, report, "a:known", "b:known")

	if got := report.Models["fable"]; got != "b" {
		t.Fatalf("models = %v, want Fable requests to go to b", report.Models)
	}
}

func TestFableRequestsAlsoCheckTheSharedLimits(t *testing.T) {
	r, now := testRouter()

	observeReset(r, "b", now.Add(3*24*time.Hour))
	observeClaude(r, "a", http.Header{
		"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"},
		"Anthropic-Ratelimit-Unified-5h-Reset":  {unix(now.Add(4 * time.Hour))},
		"Anthropic-Ratelimit-Unified-7d-Reset":  {unix(now.Add(24 * time.Hour))},
	})

	fable := modelRequest("claude-fable-5-2", "a", "b")
	other := modelRequest("claude-opus-5-5", "a", "b")

	wantPick(t, r, fable, "b")
	wantPick(t, r, other, "b")

	*now = now.Add(5 * time.Minute)
	wantPick(t, r, fable, "a")
	wantPick(t, r, other, "b")
}

func TestStatusShowsTheOrderForOtherModels(t *testing.T) {
	r, now := testRouter()

	observeReset(r, "b", now.Add(3*24*time.Hour))
	observeClaude(r, "a", fableUsedUp(now.Add(24*time.Hour), now.Add(24*time.Hour)))

	// CPA leaves a out of Fable requests while it pauses that model there.
	wantPick(t, r, modelRequest("claude-fable-5-2", "a", "b"), "b")
	wantPick(t, r, modelRequest("claude-fable-5-2", "b"), "b")

	// Until a request for another model arrives, which accounts CPA offers for it is unknown, so a still counts.
	wantAccounts(t, r.Status().Providers[0], "a:known", "b:known")

	// CPA leaves a out of requests for other models too, for example during a cooldown.
	wantPick(t, r, modelRequest("claude-opus-5-5", "b"), "b")
	wantAccounts(t, r.Status().Providers[0], "b:known", "a:known:unavailable")

	// Fable requests don't change the order shown for other models.
	wantPick(t, r, modelRequest("claude-fable-5-2", "a", "b"), "b")
	wantAccounts(t, r.Status().Providers[0], "b:known", "a:known:unavailable")
}

func TestFableAliasIsLearnedFromItsModel(t *testing.T) {
	r, now := testRouter()

	observeReset(r, "b", now.Add(3*24*time.Hour))
	observeClaude(r, "a", fableUsedUp(now.Add(24*time.Hour), now.Add(24*time.Hour)))

	alias := modelRequest("deep-thinker", "a", "b")
	wantPick(t, r, alias, "a")

	r.Observe(pluginapi.UsageRecord{Provider: "claude", AuthID: "b", Alias: "Deep-Thinker", Model: "claude-fable-5-2"})
	wantPick(t, r, alias, "b")

	r.Observe(pluginapi.UsageRecord{Provider: "claude", AuthID: "b", Alias: "deep-thinker", Model: "claude-opus-5-5"})
	wantPick(t, r, alias, "a")

	// A name with "fable" in it can be an alias of another model.
	named := modelRequest("fable-alternative", "a", "b")
	wantPick(t, r, named, "b")

	r.Observe(pluginapi.UsageRecord{Provider: "claude", AuthID: "a", Alias: "fable-alternative", Model: "claude-opus-5-5"})
	wantPick(t, r, named, "a")
}

func TestFableLimitWithoutItsOwnResetUsesTheWeeklyReset(t *testing.T) {
	r, now := testRouter()

	weeklyReset := now.Add(24 * time.Hour)
	observeReset(r, "b", now.Add(3*24*time.Hour))
	observeClaude(r, "a", http.Header{
		"Anthropic-Ratelimit-Unified-7d-Reset":          {unix(weeklyReset)},
		"Anthropic-Ratelimit-Unified-7d_oi-Status":      {"rejected"},
		"Anthropic-Ratelimit-Unified-7d_oi-Utilization": {"1.0"},
	})

	wantPick(t, r, modelRequest("claude-fable-5-2", "a", "b"), "b")
	wantPick(t, r, modelRequest("claude-opus-5-5", "a", "b"), "a")

	for _, usage := range r.Status().Providers[0].Accounts[0].Windows {
		if usage.Model == "fable" && (!usage.Limited || usage.ResetsAt == nil || !usage.ResetsAt.Equal(weeklyReset)) {
			t.Fatalf("Fable window = %+v, want limited until the weekly reset", usage)
		}
	}
}

func TestFableResetOrdersAnAccountWithoutAWeeklyReset(t *testing.T) {
	r, now := testRouter()

	observeReset(r, "b", now.Add(3*24*time.Hour))
	observeClaude(r, "a", http.Header{
		"Anthropic-Ratelimit-Unified-7d-Utilization":    {"0.81"},
		"Anthropic-Ratelimit-Unified-7d_oi-Utilization": {"0.5"},
		"Anthropic-Ratelimit-Unified-7d_oi-Reset":       {unix(now.Add(24 * time.Hour))},
	})

	wantPick(t, r, modelRequest("claude-opus-5-5", "a", "b"), "a")
	wantPick(t, r, modelRequest("claude-fable-5-2", "a", "b"), "a")

	// The weekly limit used up, with only the Fable reset given, holds a until that reset.
	observeClaude(r, "a", http.Header{
		"Anthropic-Ratelimit-Unified-7d-Status":         {"rejected"},
		"Anthropic-Ratelimit-Unified-7d-Utilization":    {"1.0"},
		"Anthropic-Ratelimit-Unified-7d_oi-Utilization": {"0.5"},
		"Anthropic-Ratelimit-Unified-7d_oi-Reset":       {unix(now.Add(24 * time.Hour))},
	})

	wantPick(t, r, modelRequest("claude-opus-5-5", "a", "b"), "b")
	wantPick(t, r, modelRequest("claude-fable-5-2", "a", "b"), "b")
}

func TestFableLimitWithoutAnyResetUsesTheAccountsWeeklyReset(t *testing.T) {
	r, now := testRouter()

	weeklyReset := now.Add(24 * time.Hour)
	observeReset(r, "b", now.Add(3*24*time.Hour))
	observeReset(r, "a", weeklyReset)

	// Anthropic can reject Fable requests without sending any reset.
	rejected := func(requestedAt time.Time) pluginapi.UsageRecord {
		return pluginapi.UsageRecord{
			Provider:    "claude",
			AuthID:      "a",
			RequestedAt: requestedAt,
			Failed:      true,
			ResponseHeaders: http.Header{
				"Anthropic-Ratelimit-Unified-Status":            {"rejected"},
				"Anthropic-Ratelimit-Unified-7d-Status":         {"allowed"},
				"Anthropic-Ratelimit-Unified-7d-Utilization":    {"0.69"},
				"Anthropic-Ratelimit-Unified-7d_oi-Status":      {"rejected"},
				"Anthropic-Ratelimit-Unified-7d_oi-Utilization": {"1.02"},
			},
		}
	}

	fable := modelRequest("claude-fable-5-2", "a", "b")

	// A response to a request sent before a's weekly window began belongs to the window before it.
	r.Observe(rejected(weeklyReset.Add(-7*24*time.Hour - time.Minute)))
	wantPick(t, r, fable, "a")

	r.Observe(rejected(*now))
	wantPick(t, r, fable, "b")
	wantPick(t, r, modelRequest("claude-opus-5-5", "a", "b"), "a")
}

func TestDelayedFableRejectionDoesNotHoldTheNextWeek(t *testing.T) {
	r, now := testRouter()

	// a's weekly window began a day ago.
	weeklyReset := now.Add(6 * 24 * time.Hour)
	observeReset(r, "b", now.Add(7*24*time.Hour))
	r.Observe(pluginapi.UsageRecord{
		Provider:    "claude",
		AuthID:      "a",
		RequestedAt: *now,
		ResponseHeaders: http.Header{
			"Anthropic-Ratelimit-Unified-7d-Utilization":    {"0.1"},
			"Anthropic-Ratelimit-Unified-7d-Reset":          {unix(weeklyReset)},
			"Anthropic-Ratelimit-Unified-7d_oi-Status":      {"allowed"},
			"Anthropic-Ratelimit-Unified-7d_oi-Utilization": {"0.2"},
			"Anthropic-Ratelimit-Unified-7d_oi-Reset":       {unix(weeklyReset)},
		},
	})

	// A rejection from the week before arrives late, without any reset.
	r.Observe(pluginapi.UsageRecord{
		Provider:    "claude",
		AuthID:      "a",
		RequestedAt: now.Add(-25 * time.Hour),
		Failed:      true,
		ResponseHeaders: http.Header{
			"Anthropic-Ratelimit-Unified-Status":            {"rejected"},
			"Anthropic-Ratelimit-Unified-7d_oi-Status":      {"rejected"},
			"Anthropic-Ratelimit-Unified-7d_oi-Utilization": {"1.02"},
		},
	})

	wantPick(t, r, modelRequest("claude-fable-5-2", "a", "b"), "a")
}

func TestCodexUsageWithoutResetCountsDuringALongStream(t *testing.T) {
	r, now := testRouter()

	start := *now
	observeCodex(r, "y", http.Header{
		"X-Codex-Secondary-Window-Minutes": {"10080"},
		"X-Codex-Secondary-Used-Percent":   {"10"},
		"X-Codex-Secondary-Reset-At":       {unix(start.Add(3 * 24 * time.Hour))},
	})

	// A stream that began with x's 5-hour window ends 20 minutes later, so its relative reset lands 20 minutes late.
	*now = start.Add(20 * time.Minute)
	r.Observe(pluginapi.UsageRecord{
		Provider:    "codex",
		AuthID:      "x",
		RequestedAt: start,
		ResponseHeaders: http.Header{
			"X-Codex-Primary-Window-Minutes":      {"300"},
			"X-Codex-Primary-Used-Percent":        {"40"},
			"X-Codex-Primary-Reset-After-Seconds": {"18000"},
		},
	})

	// A request sent 10 minutes into that window reports it used up, without a reset.
	*now = start.Add(25 * time.Minute)
	r.Observe(pluginapi.UsageRecord{
		Provider:    "codex",
		AuthID:      "x",
		RequestedAt: start.Add(10 * time.Minute),
		ResponseHeaders: http.Header{
			"X-Codex-Primary-Window-Minutes": {"300"},
			"X-Codex-Primary-Used-Percent":   {"100"},
		},
	})

	wantPick(t, r, codexRequest("x", "y"), "y")
}

func TestAllowedFableStatusWithoutUsageLiftsTheLimit(t *testing.T) {
	r, now := testRouter()

	weeklyReset := now.Add(24 * time.Hour)
	observeReset(r, "b", now.Add(3*24*time.Hour))
	r.Observe(pluginapi.UsageRecord{Provider: "claude", AuthID: "a", RequestedAt: *now, ResponseHeaders: fableUsedUp(weeklyReset, weeklyReset)})

	fable := modelRequest("claude-fable-5-2", "a", "b")
	allowed := func(requestedAt time.Time) pluginapi.UsageRecord {
		return pluginapi.UsageRecord{
			Provider:    "claude",
			AuthID:      "a",
			RequestedAt: requestedAt,
			ResponseHeaders: http.Header{
				"Anthropic-Ratelimit-Unified-7d-Reset":     {unix(weeklyReset)},
				"Anthropic-Ratelimit-Unified-7d_oi-Status": {"allowed"},
			},
		}
	}

	// A response to a request sent before the limit arrived can't lift it.
	r.Observe(allowed(now.Add(-time.Minute)))
	wantPick(t, r, fable, "b")

	// A warning without usage says the window is nearly used up, so even a newer one can't lift the limit.
	*now = now.Add(time.Second)
	warning := allowed(*now)
	warning.ResponseHeaders = http.Header{
		"Anthropic-Ratelimit-Unified-7d-Reset":     {unix(weeklyReset)},
		"Anthropic-Ratelimit-Unified-7d_oi-Status": {"allowed_warning"},
	}
	r.Observe(warning)
	wantPick(t, r, fable, "b")

	// After a manual reset, the five-minute check finds Fable allowed, and an unused window reports no usage.
	*now = now.Add(5 * time.Minute)
	wantPick(t, r, fable, "a")
	r.Observe(allowed(*now))
	wantPick(t, r, fable, "a")
	wantPick(t, r, fable, "a")

	// An allowed status alone doesn't add a window an account never reported.
	r.Observe(pluginapi.UsageRecord{Provider: "claude", AuthID: "b", RequestedAt: *now, ResponseHeaders: http.Header{
		"Anthropic-Ratelimit-Unified-5h-Status": {"allowed"},
	}})

	for _, pool := range r.Status().Providers {
		for _, account := range pool.Accounts {
			if account.ID == "b" && len(account.Windows) != 0 {
				t.Fatalf("b windows = %+v, want none", account.Windows)
			}
		}
	}
}

func TestStatusSaysWhenFableRequestsCanGoAgain(t *testing.T) {
	r, now := testRouter()

	tuesday, wednesday := now.Add(24*time.Hour), now.Add(2*24*time.Hour)
	observeClaude(r, "a", fableUsedUp(wednesday, wednesday))
	observeClaude(r, "b", fableUsedUp(tuesday, tuesday))
	observeReset(r, "c", now.Add(4*24*time.Hour))

	// CPA leaves b out of requests for other models, and c out of Fable requests.
	wantPick(t, r, modelRequest("claude-opus-5-5", "a"), "a")
	wantPick(t, r, modelRequest("claude-fable-5-2", "a", "b"), "b")

	report := r.Status().Providers[0]
	if got, ok := report.Models["fable"]; !ok || got != "" {
		t.Fatalf("models = %v, want no account for Fable requests", report.Models)
	}

	if got := report.ModelsUntil["fable"]; !got.Equal(tuesday) {
		t.Fatalf("models until = %v, want Fable requests back when b's limit resets", report.ModelsUntil)
	}

	// CPA's retries leave out the accounts already tried, so the last Fable offer can hold only the one that frees up last.
	wantPick(t, r, modelRequest("claude-opus-5-5", "a", "b"), "b")
	wantPick(t, r, modelRequest("claude-fable-5-2", "a"), "a")

	if got := r.Status().Providers[0].ModelsUntil["fable"]; !got.Equal(tuesday) {
		t.Fatalf("models until = %v, want b's reset after a retry", got)
	}

	// A retry of a request for another model leaves b out too; b can still take Fable requests on Tuesday.
	wantPick(t, r, modelRequest("claude-opus-5-5", "a"), "a")

	if got := r.Status().Providers[0].ModelsUntil["fable"]; !got.Equal(tuesday) {
		t.Fatalf("models until = %v, want b's reset after a retry for another model", got)
	}

	// c takes Fable requests, then CPA pauses it for them, for a time it doesn't report.
	wantPick(t, r, modelRequest("claude-opus-5-5", "a", "b", "c"), "b")
	wantPick(t, r, modelRequest("claude-fable-5-2", "a", "b", "c"), "c")
	wantPick(t, r, modelRequest("claude-fable-5-2", "a", "b"), "b")

	if got := r.Status().Providers[0].ModelsUntil; got != nil {
		t.Fatalf("models until = %v, want no time while CPA pauses c", got)
	}

	// A retry of a request for another model leaves c out as well, which says nothing about when c takes Fable again.
	wantPick(t, r, modelRequest("claude-opus-5-5", "a", "b"), "b")

	if got := r.Status().Providers[0].ModelsUntil; got != nil {
		t.Fatalf("models until = %v, want no time while CPA pauses c", got)
	}
}

func TestFableTimeLeavesOutAccountsCPANeverOffersForFable(t *testing.T) {
	r, now := testRouter()

	tuesday := now.Add(24 * time.Hour)
	observeClaude(r, "b", fableUsedUp(tuesday, tuesday))

	// CPA offers d only for other models, and d's 5-hour limit frees up in an hour.
	observeClaude(r, "d", http.Header{
		"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"},
		"Anthropic-Ratelimit-Unified-5h-Reset":  {unix(now.Add(time.Hour))},
		"Anthropic-Ratelimit-Unified-7d-Reset":  {unix(now.Add(2 * 24 * time.Hour))},
	})

	// CPA offers e only for other models too, and nothing holds it.
	observeReset(r, "e", now.Add(5*24*time.Hour))

	wantPick(t, r, modelRequest("claude-opus-5-5", "b", "d", "e"), "b")
	wantPick(t, r, modelRequest("claude-fable-5-2", "b"), "b")

	if got := r.Status().Providers[0].ModelsUntil["fable"]; !got.Equal(tuesday) {
		t.Fatalf("models until = %v, want b's Fable reset", got)
	}
}

func TestLateRejectionHoldsAgainstAnOlderStream(t *testing.T) {
	r, now := testRouter()

	weeklyReset := now.Add(24 * time.Hour)
	observeReset(r, "b", now.Add(3*24*time.Hour))

	// A stream sent ten minutes ago ends now, reporting half of a's Fable used.
	start := *now
	r.Observe(pluginapi.UsageRecord{Provider: "claude", AuthID: "a", RequestedAt: start.Add(-10 * time.Minute), ResponseHeaders: http.Header{
		"Anthropic-Ratelimit-Unified-7d-Reset":          {unix(weeklyReset)},
		"Anthropic-Ratelimit-Unified-7d_oi-Status":      {"allowed"},
		"Anthropic-Ratelimit-Unified-7d_oi-Utilization": {"0.5"},
		"Anthropic-Ratelimit-Unified-7d_oi-Reset":       {unix(weeklyReset)},
	}})

	// A rejection to a request sent a minute ago arrives next.
	*now = start.Add(2 * time.Minute)
	r.Observe(pluginapi.UsageRecord{Provider: "claude", AuthID: "a", RequestedAt: start.Add(-time.Minute), Failed: true, ResponseHeaders: fableUsedUp(weeklyReset, weeklyReset)})

	fable := modelRequest("claude-fable-5-2", "a", "b")
	wantPick(t, r, fable, "b")

	// Another stream, sent before the rejection arrived, ends with the usage it saw when it began.
	*now = start.Add(3 * time.Minute)
	r.Observe(pluginapi.UsageRecord{Provider: "claude", AuthID: "a", RequestedAt: start.Add(time.Minute), ResponseHeaders: http.Header{
		"Anthropic-Ratelimit-Unified-7d-Reset":          {unix(weeklyReset)},
		"Anthropic-Ratelimit-Unified-7d_oi-Status":      {"allowed"},
		"Anthropic-Ratelimit-Unified-7d_oi-Utilization": {"0.6"},
		"Anthropic-Ratelimit-Unified-7d_oi-Reset":       {unix(weeklyReset)},
	}})

	wantPick(t, r, fable, "b")
}

func TestOlderResponseDoesNotHideACheck(t *testing.T) {
	r, now := testRouter()

	weeklyReset := now.Add(24 * time.Hour)
	observeReset(r, "b", now.Add(3*24*time.Hour))

	// Two requests go to a. The first one's response shows its Fable limit used up a minute later.
	firstSent, secondSent := *now, now.Add(30*time.Second)
	*now = now.Add(time.Minute)
	r.Observe(pluginapi.UsageRecord{Provider: "claude", AuthID: "a", RequestedAt: firstSent, ResponseHeaders: fableUsedUp(weeklyReset, weeklyReset)})

	fable := modelRequest("claude-fable-5-2", "a", "b")
	wantPick(t, r, fable, "b")

	// After a manual reset, the check goes out, and the second request's older response arrives before the check's.
	*now = now.Add(5 * time.Minute)
	checkSent := *now
	wantPick(t, r, fable, "a")

	*now = now.Add(time.Second)
	r.Observe(pluginapi.UsageRecord{Provider: "claude", AuthID: "a", RequestedAt: secondSent, ResponseHeaders: fableUsedUp(weeklyReset, weeklyReset)})

	*now = now.Add(time.Second)
	r.Observe(pluginapi.UsageRecord{Provider: "claude", AuthID: "a", RequestedAt: checkSent, ResponseHeaders: http.Header{
		"Anthropic-Ratelimit-Unified-7d_oi-Status":      {"allowed"},
		"Anthropic-Ratelimit-Unified-7d_oi-Utilization": {"0.0"},
		"Anthropic-Ratelimit-Unified-7d_oi-Reset":       {unix(weeklyReset)},
	}})

	wantPick(t, r, fable, "a")
}

func TestStatusNamesWhereFableRequestsGo(t *testing.T) {
	r, now := testRouter()

	observeReset(r, "b", now.Add(2*24*time.Hour))
	observeReset(r, "c", now.Add(3*24*time.Hour))
	observeClaude(r, "a", fableUsedUp(now.Add(24*time.Hour), now.Add(24*time.Hour)))

	wantPick(t, r, modelRequest("claude-opus-5-5", "a", "b", "c"), "a")

	if got := r.Status().Providers[0].Models; got["fable"] != "b" {
		t.Fatalf("models = %v, want Fable requests to go to b", got)
	}

	// CPA leaves b out of Fable requests, for example while it pauses that model there.
	wantPick(t, r, modelRequest("claude-fable-5-2", "a", "c"), "c")

	report := r.Status().Providers[0]
	if got := report.Models; len(got) != 1 || got["fable"] != "c" {
		t.Fatalf("models = %v, want Fable requests to go to c", got)
	}

	wantAccounts(t, report, "a:known", "b:known", "c:known")

	observeClaude(r, "c", fableUsedUp(now.Add(3*24*time.Hour), now.Add(3*24*time.Hour)))

	if got, ok := r.Status().Providers[0].Models["fable"]; !ok || got != "" {
		t.Fatalf("models = %v, want no account for Fable requests", r.Status().Providers[0].Models)
	}
}

func TestStatusNamesNoFableDestinationWithoutFable(t *testing.T) {
	r, now := testRouter()

	observeReset(r, "a", now.Add(24*time.Hour))
	wantPick(t, r, modelRequest("claude-opus-5-5", "a"), "a")

	observeCodex(r, "x", http.Header{
		"X-Codex-Secondary-Used-Percent":   {"40"},
		"X-Codex-Secondary-Window-Minutes": {"10080"},
		"X-Codex-Secondary-Reset-At":       {unix(now.Add(24 * time.Hour))},
	})
	wantPick(t, r, codexRequest("x"), "x")

	// Only Claude accounts report a Fable limit, so a Codex model's name decides nothing.
	codexAlias := codexRequest("x")
	codexAlias.Model = "fable-alternative"
	wantPick(t, r, codexAlias, "x")

	for _, pool := range r.Status().Providers {
		if pool.Models != nil {
			t.Fatalf("%s models = %v, want none", pool.Provider, pool.Models)
		}
	}
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
	wantPick(t, r, req, "a")
	wantPick(t, r, req, "b")

	*now = requestedAt.Add(time.Hour)
	wantPick(t, r, req, "a")
}

func TestUsedUpAccountIsCheckedAgainEveryFewMinutes(t *testing.T) {
	r, now := testRouter()

	observeReset(r, "b", now.Add(6*24*time.Hour))

	rejected := pluginapi.UsageRecord{
		Provider:    "claude",
		AuthID:      "a",
		RequestedAt: *now,
		Failed:      true,
		ResponseHeaders: http.Header{
			"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"},
			"Anthropic-Ratelimit-Unified-5h-Reset":  {unix(now.Add(4 * time.Hour))},
		},
	}
	r.Observe(rejected)

	req := pickRequest("a", "b")
	wantPick(t, r, req, "b")

	*now = now.Add(limitRecheck - time.Second)
	wantPick(t, r, req, "b")

	*now = now.Add(time.Second)

	for _, account := range r.Status().Providers[0].Accounts {
		if account.ID == "a" && account.State != "limited" {
			t.Fatalf("account due for a check = %+v", account)
		}
	}

	wantPick(t, r, req, "a")
	wantPick(t, r, req, "b")

	rejected.RequestedAt = *now
	r.Observe(rejected)

	*now = now.Add(limitRecheck - time.Second)
	wantPick(t, r, req, "b")

	*now = now.Add(time.Second)
	wantPick(t, r, req, "a")
}

func TestManualResetIsLearnedFromTheNextResponse(t *testing.T) {
	output := captureLogs(t)
	r, now := testRouter()

	weeklyReset := now.Add(3 * 24 * time.Hour)
	observeReset(r, "b", now.Add(6*24*time.Hour))

	limitedAt := *now
	r.Observe(pluginapi.UsageRecord{
		Provider:    "claude",
		AuthID:      "a",
		RequestedAt: limitedAt.Add(-time.Second),
		Failed:      true,
		ResponseHeaders: http.Header{
			"Anthropic-Ratelimit-Unified-7d-Status":         {"rejected"},
			"Anthropic-Ratelimit-Unified-7d-Utilization":    {"1.0"},
			"Anthropic-Ratelimit-Unified-7d-Reset":          {unix(weeklyReset)},
			"Anthropic-Ratelimit-Unified-7d_oi-Utilization": {"0.65"},
			"Anthropic-Ratelimit-Unified-7d_oi-Reset":       {unix(weeklyReset)},
		},
	})

	req := pickRequest("a", "b")
	wantPick(t, r, req, "b")

	*now = now.Add(time.Minute)
	r.Observe(pluginapi.UsageRecord{
		Provider:    "claude",
		AuthID:      "a",
		RequestedAt: limitedAt.Add(-time.Minute),
		ResponseHeaders: http.Header{
			"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.98"},
			"Anthropic-Ratelimit-Unified-7d-Reset":       {unix(weeklyReset)},
		},
	})
	wantPick(t, r, req, "b")

	*now = limitedAt.Add(limitRecheck)
	wantPick(t, r, req, "a")

	r.Observe(pluginapi.UsageRecord{
		Provider:    "claude",
		AuthID:      "a",
		RequestedAt: *now,
		ResponseHeaders: http.Header{
			"Anthropic-Ratelimit-Unified-7d-Utilization":    {"0"},
			"Anthropic-Ratelimit-Unified-7d-Reset":          {unix(weeklyReset)},
			"Anthropic-Ratelimit-Unified-7d_oi-Utilization": {"0"},
			"Anthropic-Ratelimit-Unified-7d_oi-Reset":       {unix(weeklyReset)},
		},
	})
	wantPick(t, r, req, "a")

	*now = now.Add(time.Minute)
	r.Observe(pluginapi.UsageRecord{
		Provider:    "claude",
		AuthID:      "a",
		RequestedAt: limitedAt.Add(-2 * time.Minute),
		ResponseHeaders: http.Header{
			"Anthropic-Ratelimit-Unified-7d-Utilization":    {"1"},
			"Anthropic-Ratelimit-Unified-7d-Reset":          {unix(weeklyReset)},
			"Anthropic-Ratelimit-Unified-7d_oi-Utilization": {"0.7"},
			"Anthropic-Ratelimit-Unified-7d_oi-Reset":       {unix(weeklyReset)},
		},
	})
	wantPick(t, r, req, "a")

	pool := r.Status().Providers[0]
	wantAccounts(t, pool, "a:known", "b:known")

	for _, usage := range pool.Accounts[0].Windows {
		if usage.UsedPercent != 0 || usage.Limited || usage.ResetsAt == nil || !usage.ResetsAt.Equal(weeklyReset) {
			t.Fatalf("window after the reset = %+v", usage)
		}
	}

	if got := strings.Count(output.String(), `auth "a" is below its quota limits again`); got != 1 {
		t.Fatalf("logs = %q, want one return", output.String())
	}
}

func TestCodexCheckReplacesAResetEstimatedDuringALongStream(t *testing.T) {
	r, now := testRouter()

	streamedAt := *now
	weeklyReset := streamedAt.Add(24 * time.Hour)

	observeCodex(r, "b", http.Header{
		"X-Codex-Secondary-Window-Minutes": {"10080"},
		"X-Codex-Secondary-Reset-At":       {unix(weeklyReset.Add(15 * time.Minute))},
	})

	longStream := pluginapi.UsageRecord{
		Provider:    "codex",
		AuthID:      "a",
		RequestedAt: streamedAt,
		ResponseHeaders: http.Header{
			"X-Codex-Secondary-Window-Minutes":      {"10080"},
			"X-Codex-Secondary-Used-Percent":        {"100"},
			"X-Codex-Secondary-Reset-After-Seconds": {"86400"},
		},
	}

	*now = now.Add(20 * time.Minute)
	r.Observe(longStream)

	req := codexRequest("a", "b")
	wantPick(t, r, req, "b")

	*now = now.Add(limitRecheck)
	wantPick(t, r, req, "a")

	r.Observe(pluginapi.UsageRecord{
		Provider:    "codex",
		AuthID:      "a",
		RequestedAt: *now,
		ResponseHeaders: http.Header{
			"X-Codex-Secondary-Window-Minutes": {"10080"},
			"X-Codex-Secondary-Used-Percent":   {"0"},
			"X-Codex-Secondary-Reset-At":       {unix(weeklyReset)},
		},
	})
	wantPick(t, r, req, "a")

	*now = streamedAt.Add(30 * time.Minute)
	r.Observe(longStream)
	wantPick(t, r, req, "a")

	usage := r.Status().Providers[0].Accounts[0]
	if usage.ID != "a" || usage.ResetsAt == nil || !usage.ResetsAt.Equal(weeklyReset) || len(usage.Windows) != 1 || usage.Windows[0].UsedPercent != 0 {
		t.Fatalf("account after a late stream = %+v", usage)
	}
}

func TestRequestTimeAfterNowCannotLowerUsage(t *testing.T) {
	r, now := testRouter()

	weeklyReset := now.Add(24 * time.Hour)
	observeReset(r, "b", now.Add(6*24*time.Hour))
	r.Observe(pluginapi.UsageRecord{
		Provider:    "claude",
		AuthID:      "a",
		RequestedAt: *now,
		Failed:      true,
		ResponseHeaders: http.Header{
			"Anthropic-Ratelimit-Unified-7d-Status": {"rejected"},
			"Anthropic-Ratelimit-Unified-7d-Reset":  {unix(weeklyReset)},
		},
	})

	*now = now.Add(time.Minute)
	r.Observe(pluginapi.UsageRecord{
		Provider:    "claude",
		AuthID:      "a",
		RequestedAt: now.Add(time.Hour),
		ResponseHeaders: http.Header{
			"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.1"},
			"Anthropic-Ratelimit-Unified-7d-Reset":       {unix(weeklyReset)},
		},
	})

	wantPick(t, r, pickRequest("a", "b"), "b")
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
