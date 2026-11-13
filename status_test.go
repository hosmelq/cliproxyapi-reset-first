package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func withPriority(candidate pluginapi.SchedulerAuthCandidate, priority int) pluginapi.SchedulerAuthCandidate {
	candidate.Priority = priority
	return candidate
}

func wantAccounts(t *testing.T, pool providerReport, want ...string) {
	t.Helper()

	got := make([]string, 0, len(pool.Accounts))
	for _, account := range pool.Accounts {
		state := account.ID + ":" + account.State
		if !account.Available {
			state += ":unavailable"
		}

		got = append(got, state)
	}

	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("accounts = %v, want %v", got, want)
	}
}

func TestStatusListsAvailableAccountsFirst(t *testing.T) {
	r, now := testRouter()

	wantPick(t, r, request(subscription("a"), subscription("b"), subscription("c"), apiKey("k")), "a")

	observeReset(r, "a", now.Add(6*24*time.Hour))
	observeReset(r, "b", now.Add(24*time.Hour))
	r.Observe(pluginapi.UsageRecord{Provider: "claude", AuthID: "c", Failed: true})

	wantPick(t, r, request(subscription("a"), subscription("c"), apiKey("k")), "a")

	report := r.Status()
	if report.Version != version || len(report.Providers) != 1 {
		t.Fatalf("report = %+v", report)
	}

	pool := report.Providers[0]
	if pool.Provider != "claude" || pool.Serving != "b" {
		t.Fatalf("pool = %+v", pool)
	}

	wantAccounts(t, pool, "a:known", "c:waiting", "k:api_key", "b:known:unavailable")

	if got := pool.Accounts[3].ResetsAt; got == nil || !got.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("b resets at %v", got)
	}

	if got := pool.Accounts[1].RetryAt; got == nil || !got.Equal(now.Add(probeRetry)) {
		t.Fatalf("c retries at %v", got)
	}
}

func TestStatusSortsUnavailableAccountsByPriority(t *testing.T) {
	r, now := testRouter()

	r.Pick(request(withPriority(subscription("a"), 1), withPriority(subscription("b"), 1)))
	observeReset(r, "a", now.Add(6*24*time.Hour))
	observeReset(r, "b", now.Add(5*24*time.Hour))

	r.Pick(request(subscription("c"), subscription("d")))
	observeReset(r, "c", now.Add(4*24*time.Hour))
	observeReset(r, "d", now.Add(24*time.Hour))

	r.Pick(request(withPriority(subscription("e"), 2)))

	pool := r.Status().Providers[0]

	wantAccounts(t, pool, "e:learning", "b:known:unavailable", "a:known:unavailable", "d:known:unavailable", "c:known:unavailable")

	if pool.Accounts[0].Priority != 2 || pool.Accounts[1].Priority != 1 || pool.Accounts[3].Priority != 0 {
		t.Fatalf("priorities = %+v", pool.Accounts)
	}
}

func TestStatusServingFollowsSuccessfulUsage(t *testing.T) {
	r, now := testRouter()

	wantPick(t, r, pickRequest("a"), "a")
	observeReset(r, "a", now.Add(24*time.Hour))

	if serving := r.Status().Providers[0].Serving; serving != "a" {
		t.Fatalf("serving = %q, want a", serving)
	}

	r.Observe(pluginapi.UsageRecord{Provider: "claude", AuthID: "k"})
	r.Observe(pluginapi.UsageRecord{Provider: "claude", AuthID: "a", Failed: true})

	if serving := r.Status().Providers[0].Serving; serving != "k" {
		t.Fatalf("serving = %q, want k", serving)
	}
}

func TestStatusListsAPIKeyPoolsButNotObservedOnlyAccounts(t *testing.T) {
	r, now := testRouter()

	observeReset(r, "x", now.Add(24*time.Hour))

	if report := r.Status(); len(report.Providers) != 0 {
		t.Fatalf("providers = %+v, want none", report.Providers)
	}

	if got := r.Pick(request(apiKey("k"))); got.Handled {
		t.Fatalf("Pick() = %+v, want unhandled", got)
	}

	wantAccounts(t, r.Status().Providers[0], "k:api_key")
}

func TestStatusReportsUsageWindows(t *testing.T) {
	r, now := testRouter()

	wantPick(t, r, pickRequest("a"), "a")
	observeClaude(r, "a", http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.42"},
		"Anthropic-Ratelimit-Unified-5h-Reset":       {unix(now.Add(2 * time.Hour))},
		"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.7351"},
		"Anthropic-Ratelimit-Unified-7d-Reset":       {unix(now.Add(24 * time.Hour))},
	})

	account := r.Status().Providers[0].Accounts[0]
	if account.State != "known" || len(account.Windows) != 2 {
		t.Fatalf("account = %+v", account)
	}

	fiveHour, weekly := account.Windows[0], account.Windows[1]
	if fiveHour.Minutes != 300 || fiveHour.UsedPercent != 42 || fiveHour.Limited || fiveHour.ResetsAt == nil || !fiveHour.ResetsAt.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("5h window = %+v", fiveHour)
	}

	if weekly.Minutes != 10080 || weekly.UsedPercent != 73.5 || weekly.ResetsAt == nil || !weekly.ResetsAt.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("weekly window = %+v", weekly)
	}

	*now = now.Add(2 * time.Hour)

	fiveHour = r.Status().Providers[0].Accounts[0].Windows[0]
	if fiveHour.UsedPercent != 0 || fiveHour.ResetsAt != nil {
		t.Fatalf("5h window after its reset = %+v", fiveHour)
	}
}

func TestStatusReportsUsedUpAccounts(t *testing.T) {
	r, now := testRouter()

	wantPick(t, r, pickRequest("a", "b"), "a")
	observeReset(r, "b", now.Add(6*24*time.Hour))
	observeClaude(r, "a", http.Header{
		"Anthropic-Ratelimit-Unified-5h-Status":      {"rejected"},
		"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.98"},
		"Anthropic-Ratelimit-Unified-5h-Reset":       {unix(now.Add(2 * time.Hour))},
		"Anthropic-Ratelimit-Unified-7d-Reset":       {unix(now.Add(24 * time.Hour))},
	})

	pool := r.Status().Providers[0]
	wantAccounts(t, pool, "b:known", "a:limited")

	used := pool.Accounts[1]
	if used.LimitedUntil == nil || !used.LimitedUntil.Equal(now.Add(2*time.Hour)) || used.ResetsAt == nil || !used.ResetsAt.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("used up account = %+v", used)
	}

	if len(used.Windows) != 1 || !used.Windows[0].Limited || used.Windows[0].UsedPercent != 100 {
		t.Fatalf("windows = %+v", used.Windows)
	}
}

func TestStatusIgnoresUsageFromAnEarlierWindow(t *testing.T) {
	r, now := testRouter()

	wantPick(t, r, pickRequest("a"), "a")
	observeClaude(r, "a", http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.1"},
		"Anthropic-Ratelimit-Unified-5h-Reset":       {unix(now.Add(5 * time.Hour))},
	})
	observeClaude(r, "a", http.Header{
		"Anthropic-Ratelimit-Unified-5h-Status":      {"rejected"},
		"Anthropic-Ratelimit-Unified-5h-Utilization": {"1"},
		"Anthropic-Ratelimit-Unified-5h-Reset":       {unix(now.Add(-time.Minute))},
	})

	windows := r.Status().Providers[0].Accounts[0].Windows
	if len(windows) != 1 || windows[0].UsedPercent != 10 || windows[0].Limited || windows[0].ResetsAt == nil || !windows[0].ResetsAt.Equal(now.Add(5*time.Hour)) {
		t.Fatalf("windows = %+v", windows)
	}
}

func TestStatusReportsRejectedWindowWithInvalidUtilization(t *testing.T) {
	r, now := testRouter()

	wantPick(t, r, pickRequest("a"), "a")
	observeClaude(r, "a", http.Header{
		"Anthropic-Ratelimit-Unified-5h-Status":      {"rejected"},
		"Anthropic-Ratelimit-Unified-5h-Utilization": {"NaN"},
		"Anthropic-Ratelimit-Unified-5h-Reset":       {unix(now.Add(time.Hour))},
	})

	resp := handleManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: statusRoute}, r)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var report statusReport
	if err := json.Unmarshal(resp.Body, &report); err != nil {
		t.Fatal(err)
	}

	account := report.Providers[0].Accounts[0]
	if account.State != "limited" || len(account.Windows) != 1 || account.Windows[0].UsedPercent != 100 {
		t.Fatalf("account = %+v", account)
	}
}

func TestStatusRouteRequiresGetAndReturnsJSON(t *testing.T) {
	r, now := testRouter()

	wantPick(t, r, pickRequest("a"), "a")
	observeReset(r, "a", now.Add(24*time.Hour))

	resp := handleManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: statusRoute}, r)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Headers.Get("Content-Type"), "application/json") || resp.Headers.Get("Cache-Control") != "no-store" {
		t.Fatalf("response = %+v", resp)
	}

	var report statusReport
	if err := json.Unmarshal(resp.Body, &report); err != nil || len(report.Providers) != 1 || report.Providers[0].Accounts[0].ID != "a" {
		t.Fatalf("report = %+v, err = %v", report, err)
	}

	if resp := handleManagement(pluginapi.ManagementRequest{Method: http.MethodPost, Path: statusRoute}, r); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d", resp.StatusCode)
	}

	if resp := handleManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/management/reset-first/other"}, r); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown path status = %d", resp.StatusCode)
	}
}

func TestStatusPageIsSelfContained(t *testing.T) {
	r, _ := testRouter()

	resp := handleManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/resource/plugins/" + pluginID + "/status"}, r)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Headers.Get("Content-Type"), "text/html") {
		t.Fatalf("response = %+v", resp)
	}

	if csp := resp.Headers.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "frame-ancestors 'self'") {
		t.Fatalf("csp = %q", csp)
	}

	if !bytes.Contains(resp.Body, []byte(statusRoute)) {
		t.Fatal("page does not read the status route")
	}

	if bytes.Contains(resp.Body, []byte("http://")) || bytes.Contains(resp.Body, []byte("https://")) {
		t.Fatal("page references an external URL")
	}
}

func TestManagementRPC(t *testing.T) {
	raw, err := handleMethod(pluginabi.MethodManagementRegister, []byte(`{"BasePath":"/v0/management"}`))
	if err != nil {
		t.Fatal(err)
	}

	var envelope pluginabi.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil || !envelope.OK {
		t.Fatalf("envelope = %s, err = %v", raw, err)
	}

	var registration struct {
		Routes    []pluginapi.ResourceRoute `json:"routes"`
		Resources []pluginapi.ResourceRoute `json:"resources"`
	}
	if err := json.Unmarshal(envelope.Result, &registration); err != nil {
		t.Fatal(err)
	}

	if len(registration.Routes) != 1 || registration.Routes[0].Path != statusRoute ||
		len(registration.Resources) != 1 || registration.Resources[0].Path != "status" || registration.Resources[0].Menu == "" {
		t.Fatalf("registration = %+v", registration)
	}

	raw, err = handleMethod(pluginabi.MethodManagementHandle, []byte("{"))
	if err != nil {
		t.Fatal(err)
	}

	var response pluginapi.ManagementResponse
	if err := json.Unmarshal(raw, &envelope); err != nil || json.Unmarshal(envelope.Result, &response) != nil || response.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed handle = %s", raw)
	}
}
