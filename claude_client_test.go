package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func claudeFixture(now time.Time) map[string]any {
	return map[string]any{"cedar_ember": map[string]any{"eligible": true, "at_limit": true, "grants": []any{
		map[string]any{"id": "grant_fable", "resets_left": 2, "resets_total": 3,
			"starts_at": now.Add(-time.Hour).Format(time.RFC3339), "ends_at": now.Add(5 * time.Minute).Format(time.RFC3339),
			"usable_now": true, "use_requires_limit": true, "clears": []string{"seven_day_fable"},
			"percent_used": map[string]float64{"seven_day_fable": 75, "seven_day": 100}},
	}}}
}

func parseFixture(t *testing.T, d map[string]any, now time.Time) Snapshot {
	t.Helper()
	raw, _ := json.Marshal(d)
	s, err := parseClaudeUsage(raw, now)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestClaudeEligibility(t *testing.T) {
	now := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		change  func(map[string]any, map[string]any)
		blocked bool
	}{
		{"usable", func(b, g map[string]any) {}, false},
		{"paused", func(b, g map[string]any) { g["paused"] = true }, true},
		{"blocking", func(b, g map[string]any) { g["blocking"] = []string{"seven_day"} }, true},
		{"not_usable", func(b, g map[string]any) { g["usable_now"] = false }, true},
		{"requires_limit", func(b, g map[string]any) { b["at_limit"] = false }, true},
		{"default_requires_limit", func(b, g map[string]any) { delete(g, "use_requires_limit"); b["at_limit"] = false }, true},
		{"cooldown", func(b, g map[string]any) { b["cooldown_until"] = now.Add(time.Minute).Format(time.RFC3339) }, true},
		{"zero_covered_usage", func(b, g map[string]any) { g["percent_used"] = map[string]float64{"seven_day": 100} }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := claudeFixture(now)
			b := d["cedar_ember"].(map[string]any)
			g := b["grants"].([]any)[0].(map[string]any)
			tc.change(b, g)
			s := parseFixture(t, d, now)
			if len(s.Credits) != 1 || s.AvailableCount != 2 {
				t.Fatal(s)
			}
			if (s.Credits[0].BlockedReason != "") != tc.blocked {
				t.Fatalf("blocked=%q", s.Credits[0].BlockedReason)
			}
			if s.Credits[0].ID != "grant_fable:1" {
				t.Fatal(s.Credits)
			}
		})
	}
}

func TestClaudeRejectsInvalidGrantAndDoesNotScheduleExpiredOrNonExpiring(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, mode := range []string{"expired", "no_expiry", "invalid_balance", "invalid_id", "invalid_usage"} {
		t.Run(mode, func(t *testing.T) {
			d := claudeFixture(now)
			g := d["cedar_ember"].(map[string]any)["grants"].([]any)[0].(map[string]any)
			switch mode {
			case "expired":
				g["ends_at"] = now.Add(-time.Second).Format(time.RFC3339)
			case "no_expiry":
				delete(g, "ends_at")
			case "invalid_balance":
				g["resets_left"] = 5
			case "invalid_id":
				g["id"] = "../other"
			case "invalid_usage":
				g["percent_used"] = map[string]float64{"seven_day_fable": 101}
			}
			raw, _ := json.Marshal(d)
			s, err := parseClaudeUsage(raw, now)
			if strings.HasPrefix(mode, "invalid") {
				if err == nil {
					t.Fatal("accepted malformed grant")
				}
			} else if err != nil || len(s.Credits) != 0 {
				t.Fatalf("scheduled invalid expiry: %+v %v", s, err)
			}
		})
	}
}

func TestClaudeHTTPContractAndIdempotency(t *testing.T) {
	now := time.Now().UTC()
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("anthropic-beta") != "oauth-2025-04-20" {
			t.Error("missing expected auth headers")
		}
		if r.Method == http.MethodGet {
			if r.URL.Query().Get("skip_spend") != "1" {
				t.Error("read may spend")
			}
			json.NewEncoder(w).Encode(claudeFixture(now))
			return
		}
		posts++
		if r.URL.Path != "/api/organizations/org/reset_rate_limits" {
			t.Error(r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		var d map[string]string
		json.Unmarshal(raw, &d)
		if len(d) != 3 || d["grant_id"] != "grant_fable" || d["program"] != "cedar_ember" || d["request_id"] != "stable" {
			t.Error(string(raw))
		}
		io.WriteString(w, `{"result":"already_used"}`)
	}))
	defer srv.Close()
	c := &ClaudeClient{BaseURL: srv.URL}
	creds := CodexCredentials{AccessToken: "secret", OrganizationID: "org"}
	if _, err := c.ListCredits(creds); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		r, e := c.Consume(creds, "stable", "grant_fable:1")
		if e != nil || r.Code != ConsumeCodeAlreadyRedeemed {
			t.Fatal(r, e)
		}
	}
	if posts != 2 {
		t.Fatal(posts)
	}
	if _, err := c.Consume(creds, "", "grant_fable:1"); err == nil {
		t.Fatal("empty idempotency accepted")
	}
}

func TestClaudeUnknownOutcomeIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"result":"unavailable","reason":"stamp_indeterminate"}`)
	}))
	defer srv.Close()
	_, err := (&ClaudeClient{BaseURL: srv.URL}).Consume(CodexCredentials{OrganizationID: "org"}, "stable", "grant:0")
	if err == nil || classifyConsume(ConsumeResponse{}, err) != decisionRetry {
		t.Fatal(err)
	}
}

func TestRedemptionIntentIsDurableBeforeNetworkAndSurvivesRestart(t *testing.T) {
	now := time.Now()
	clock := &fakeClock{t: now}
	cfg := Config{RefreshInterval: 10 * time.Minute, TriggerLeadTime: 10 * time.Minute}
	fake := &fakeClient{credits: map[string]*Credit{"credit": {ID: "credit", Status: "available", ExpiresAt: now.Add(time.Minute)}}, weeklyPct: 20, consumeErr: errors.New("timeout")}
	f := NewAccountFSM("a", CodexCredentials{Identity: "codex:a"}, cfg, fake, clock.Now, nil)
	path := filepath.Join(t.TempDir(), "state.json")
	f.Persist = func(r AccountRuntime) error {
		if !r.Attempt.Pending || r.Attempt.RedeemRequestID == "" || fake.consumeCalls > 0 {
			t.Fatal("intent not saved before POST")
		}
		return saveState(path, &PluginState{Accounts: map[string]AccountRuntime{"a": r}})
	}
	for i := 0; i < 4; i++ {
		f.StepAndSettle()
	}
	if fake.consumeCalls != 1 {
		t.Fatal(fake.consumeCalls)
	}
	s, _, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	r := NewAccountFSM("a", f.Creds, cfg, fake, clock.Now, nil)
	r.Restore(s.Accounts["a"])
	r.Persist = func(AccountRuntime) error { return nil }
	r.StepAndSettle()
	if len(fake.redeemIDs) != 2 || fake.redeemIDs[0] != fake.redeemIDs[1] {
		t.Fatal(fake.redeemIDs)
	}
}

func TestPersistenceFailureAndObservationNeverSpend(t *testing.T) {
	for _, observe := range []bool{false, true} {
		t.Run(map[bool]string{false: "disk_failure", true: "observe"}[observe], func(t *testing.T) {
			now := time.Now()
			fake := &fakeClient{credits: map[string]*Credit{"c": {ID: "c", Status: "available", ExpiresAt: now.Add(time.Minute)}}, weeklyPct: 0}
			f := NewAccountFSM("a", CodexCredentials{}, Config{RefreshInterval: time.Minute, TriggerLeadTime: 10 * time.Minute, ObserveOnly: observe}, fake, func() time.Time { return now }, nil)
			f.Persist = func(AccountRuntime) error { return errors.New("disk full") }
			for i := 0; i < 8; i++ {
				f.StepAndSettle()
			}
			if fake.consumeCalls != 0 {
				t.Fatal("spent reset")
			}
		})
	}
}

func TestBlockedClaudeDoesNotSpendAndGrantSlotAdvances(t *testing.T) {
	now := time.Now()
	d := claudeFixture(now)
	b := d["cedar_ember"].(map[string]any)
	g := b["grants"].([]any)[0].(map[string]any)
	g["paused"] = true
	s := parseFixture(t, d, now)
	fake := &fakeClient{credits: map[string]*Credit{s.Credits[0].ID: &s.Credits[0]}}
	f := NewAccountFSM("a", CodexCredentials{Provider: "claude"}, Config{RefreshInterval: time.Minute, TriggerLeadTime: 10 * time.Minute}, fake, func() time.Time { return now }, nil)
	for i := 0; i < 8; i++ {
		f.StepAndSettle()
	}
	if fake.consumeCalls != 0 {
		t.Fatal("spent blocked grant")
	}
	g["resets_left"] = 1
	next := parseFixture(t, d, now)
	if next.Credits[0].ID == s.Credits[0].ID {
		t.Fatal("multi-use grant unit identity reused")
	}
}

func TestClaudeCredentialsRequireAccountIdentity(t *testing.T) {
	if _, err := ExtractClaudeCredentials(json.RawMessage(`{"access_token":"secret"}`)); err == nil {
		t.Fatal("identity-free login accepted")
	}
	c, err := ExtractClaudeCredentials(json.RawMessage(`{"access_token":"secret","organization_uuid":"org","account_uuid":"account"}`))
	if err != nil || c.Identity != "claude:org:account" {
		t.Fatal(err, c.Identity)
	}
}

func TestClaudeFSMVerifiesModelSpecificGrantWithoutWeeklyRecovery(t *testing.T) {
	now := time.Now().UTC()
	d := claudeFixture(now)
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
			g := d["cedar_ember"].(map[string]any)["grants"].([]any)[0].(map[string]any)
			g["resets_left"] = 1
			g["percent_used"] = map[string]float64{"seven_day_fable": 0, "seven_day": 100}
			io.WriteString(w, `{"result":"reset"}`)
			return
		}
		json.NewEncoder(w).Encode(d)
	}))
	defer srv.Close()
	clock := &fakeClock{t: now}
	f := NewAccountFSM("a", CodexCredentials{Provider: "claude", Identity: "claude:org:account", OrganizationID: "org"}, Config{RefreshInterval: 10 * time.Minute, TriggerLeadTime: 10 * time.Minute}, &ClaudeClient{BaseURL: srv.URL}, clock.Now, nil)
	drive(t, f, clock)
	rec := f.TakePendingHistory()
	if posts != 1 || rec == nil || !rec.Success || rec.PostCredits != 1 || rec.PreWeeklyPct != rec.PostWeeklyPct {
		t.Fatalf("posts=%d record=%+v", posts, rec)
	}
}

func TestRestoreChangedIdentityAndAmbiguousRefusalHold(t *testing.T) {
	now := time.Now()
	fake := &fakeClient{credits: map[string]*Credit{"c": {ID: "c", Status: "available", ExpiresAt: now.Add(time.Hour)}}, consumeResp: ConsumeResponse{Code: ConsumeCodeNothingToReset}}
	f := NewAccountFSM("a", CodexCredentials{Identity: "new"}, Config{RefreshInterval: time.Minute}, fake, func() time.Time { return now }, nil)
	f.Restore(AccountRuntime{Identity: "old", State: StateRESETTING})
	if f.State() != StateHOLD {
		t.Fatal("restored another account's pending spend")
	}
	f.Creds.Identity = "old"
	f.state = StateRESETTING
	f.attempt = ResetAttempt{Identity: "old", TargetCreditID: "c", RedeemRequestID: "stable", Pending: true, Ambiguous: true, StartedAt: now}
	f.StepAndSettle()
	if f.State() != StateHOLD || f.Runtime().Attempt.RedeemRequestID != "stable" {
		t.Fatal("lost uncertain intent")
	}
	f.ForceConfirm()
	if f.State() != StateHOLD {
		t.Fatal("manual reset replaced uncertain intent")
	}
}
