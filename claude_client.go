package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var providerID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,200}$`)

func ExtractClaudeCredentials(raw json.RawMessage) (CodexCredentials, error) {
	var d map[string]any
	if err := json.Unmarshal(raw, &d); err != nil {
		return CodexCredentials{}, err
	}
	token, _ := lookupStringDeep(d, "access_token")
	org, _ := lookupStringDeep(d, "organization_uuid")
	account, _ := lookupStringDeep(d, "account_uuid")
	if token == "" || !providerID.MatchString(org) || !providerID.MatchString(account) {
		return CodexCredentials{}, errors.New("Claude credential needs access token, organization_uuid and account_uuid")
	}
	expiry, _ := lookupTimeDeep(d, "expired", "expires_at")
	return CodexCredentials{Provider: "claude", Identity: "claude:" + org + ":" + account, OrganizationID: org, AccessToken: token, ExpiresAt: expiry}, nil
}

// Claude uses the same scheduler contract, with one synthetic credit per remaining
// grant unit. The slot suffix prevents a retry from spending the next unit.
type ClaudeClient struct {
	BaseURL  string // Only overridden by tests.
	HTTP     *http.Client
	cached   Snapshot
	cachedAt time.Time
}

func (c *ClaudeClient) request(creds CodexCredentials, path string, payload any) ([]byte, error) {
	base := "https://api.anthropic.com"
	if c.BaseURL != "" {
		base = c.BaseURL
	}
	var body *bytes.Reader
	method := http.MethodGet
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(raw)
		method = http.MethodPost
	} else {
		body = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("User-Agent", "claude-cli/2.1.322 (external, cli)")
	req.Header.Set("x-app", "cli")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return (&OpenAIClient{HTTP: c.HTTP}).do(req)
}

func (c *ClaudeClient) ListCredits(creds CodexCredentials) (Snapshot, error) {
	raw, err := c.request(creds, "/api/oauth/usage?cedar_ember=1&skip_spend=1", nil)
	if err != nil {
		return Snapshot{}, err
	}
	snap, err := parseClaudeUsage(raw, time.Now())
	if err == nil {
		c.cached = snap
		c.cachedAt = time.Now()
	}
	return snap, err
}

func (c *ClaudeClient) GetUsage(creds CodexCredentials) (Snapshot, error) {
	if !c.cachedAt.IsZero() && time.Since(c.cachedAt) < 5*time.Second {
		return c.cached, nil
	}
	return c.ListCredits(creds)
}

func (c *ClaudeClient) Consume(creds CodexCredentials, requestID, creditID string) (ConsumeResponse, error) {
	grant, slot, ok := strings.Cut(creditID, ":")
	n, err := strconv.Atoi(slot)
	if !ok || err != nil || n < 0 || !providerID.MatchString(grant) || !providerID.MatchString(creds.OrganizationID) || requestID == "" {
		return ConsumeResponse{}, errors.New("invalid targeted Claude redemption")
	}
	raw, err := c.request(creds, "/api/organizations/"+creds.OrganizationID+"/reset_rate_limits", map[string]string{"program": "cedar_ember", "grant_id": grant, "request_id": requestID})
	if err != nil {
		return ConsumeResponse{}, err
	}
	c.cachedAt = time.Time{}
	var result struct {
		Result string `json:"result"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return ConsumeResponse{}, errors.New("invalid Claude redemption response")
	}
	if result.Reason == "stamp_indeterminate" || result.Reason == "reset_unconfirmed" {
		return ConsumeResponse{}, errors.New("Claude redemption outcome is ambiguous")
	}
	switch result.Result {
	case "reset":
		return ConsumeResponse{Code: ConsumeCodeReset}, nil
	case "already_used":
		return ConsumeResponse{Code: ConsumeCodeAlreadyRedeemed}, nil
	case "not_limited", "cooldown", "ineligible":
		return ConsumeResponse{Code: ConsumeCodeNothingToReset}, nil
	default:
		return ConsumeResponse{}, errors.New("unknown Claude redemption outcome; retain request ID")
	}
}

func parseClaudeUsage(raw []byte, now time.Time) (Snapshot, error) {
	var d struct {
		Bank *struct {
			Eligible bool       `json:"eligible"`
			Reason   string     `json:"ineligible_reason"`
			AtLimit  bool       `json:"at_limit"`
			Cooldown *time.Time `json:"cooldown_until"`
			Grants   []struct {
				ID            string             `json:"id"`
				Left          *int               `json:"resets_left"`
				Total         *int               `json:"resets_total"`
				Starts        *time.Time         `json:"starts_at"`
				Ends          *time.Time         `json:"ends_at"`
				Paused        bool               `json:"paused"`
				Blocking      []string           `json:"blocking"`
				Usable        bool               `json:"usable_now"`
				RequiresLimit *bool              `json:"use_requires_limit"`
				Unavailable   string             `json:"unavailable_reason"`
				Clears        []string           `json:"clears"`
				Used          map[string]float64 `json:"percent_used"`
			} `json:"grants"`
		} `json:"cedar_ember"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return Snapshot{}, errors.New("invalid Claude usage response")
	}
	snap := Snapshot{WeeklyPct: -1, CapturedAt: now, Limits: map[string]float64{}}
	if d.Bank == nil {
		return snap, errors.New("Claude reset grants unavailable")
	}
	if !d.Bank.Eligible {
		switch d.Bank.Reason {
		case "surface", "unavailable", "unknown", "":
			return snap, errors.New("Claude reset eligibility unavailable")
		}
		return snap, nil
	}
	for _, g := range d.Bank.Grants {
		if !providerID.MatchString(g.ID) || g.Left == nil || g.Total == nil || *g.Left < 0 || *g.Total < *g.Left {
			return snap, errors.New("invalid Claude grant balance")
		}
		for k, v := range g.Used {
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 100 {
				return snap, errors.New("invalid Claude usage percent")
			}
			snap.Limits[k] = v
			if k == "seven_day" {
				snap.WeeklyPct = 100 - int(v)
			}
		}
		if *g.Left == 0 || g.Ends == nil || !g.Ends.After(now) {
			continue
		}
		c := Credit{ID: fmt.Sprintf("%s:%d", g.ID, *g.Total-*g.Left), Status: "available", ExpiresAt: *g.Ends, Clears: g.Clears}
		if g.Starts != nil {
			c.StartsAt = *g.Starts
		}
		switch {
		case d.Bank.Cooldown != nil && d.Bank.Cooldown.After(now):
			c.BlockedReason = "provider cooldown"
		case g.Paused:
			c.BlockedReason = "paused"
		case len(g.Blocking) > 0:
			c.BlockedReason = "blocked by " + strings.Join(g.Blocking, ", ")
		case !g.Usable:
			c.BlockedReason = "not usable now"
		case (g.RequiresLimit == nil || *g.RequiresLimit) && !d.Bank.AtLimit:
			c.BlockedReason = "requires reaching a usage limit"
		default:
			hasUsage := false
			for _, k := range g.Clears {
				if g.Used[k] > 0 {
					hasUsage = true
				}
			}
			if !hasUsage {
				c.BlockedReason = "no covered usage to restore"
			}
		}
		snap.AvailableCount += *g.Left
		snap.Credits = append(snap.Credits, c)
	}
	snap.Credits = availableCreditsSorted(snap.Credits, now)
	return snap, nil
}
