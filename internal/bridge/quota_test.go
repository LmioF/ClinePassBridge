package bridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func quotaTestAuth(t *testing.T, s *Service, c Credential) []byte {
	t.Helper()
	if s.authDir == "" {
		s.authDir = t.TempDir()
	}
	c.Type, c.Label = Provider, c.ID
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(jsonBytes(c), &fields)
	fields["unknown"] = json.RawMessage(`{"serial":9007199254740993,"nested":[null,true]}`)
	raw := jsonBytes(fields)
	if err := os.WriteFile(filepath.Join(s.authDir, c.ID+".json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	quotaTestParse(t, s, c.ID, raw)
	return raw
}

func quotaTestParse(t *testing.T, s *Service, id string, raw []byte) map[string]any {
	t.Helper()
	parsed, err := s.parseAuth(jsonBytes(map[string]any{
		"FileName": id + ".json", "RawJSON": raw, "Host": map[string]any{"AuthDir": s.authDir},
	}))
	if err != nil {
		t.Fatal(err)
	}
	return parsed.(map[string]any)["Auth"].(map[string]any)
}

func quotaTestRead(t *testing.T, s *Service, id string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(s.authDir, id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func quotaTestRoom(account string, at time.Time) credentialUsage {
	percent := 3.0
	return credentialUsage{Status: "ok", accountHash: account, UpdatedAt: &at, CheckedAt: &at,
		Limits: []usageLimit{{Type: "weekly", PercentUsed: &percent}}}
}

func TestQuotaNamedLimitsAndUnknown429(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct{ message, kind string }{
		{"You have reached your weekly Clinepass limit. The limit resets in 2d 10h, please try again later.", "weekly"},
		{"You have reached your five_hour Clinepass limit", "five_hour"},
		{"You have exceeded your five-hour Cline Pass limit", "five_hour"},
		{"You have reached your monthly Clinepass limit", "monthly"},
		{"quota exhausted", "quota"},
		{"upstream request failed", ""},
		{"weekly Clinepass limit is available", ""},
		{"You have reached this team's limit for tokens per minute", ""},
	} {
		t.Run(tc.message, func(t *testing.T) {
			err := classifyUpstreamError(429, http.Header{}, map[string]any{"error": map[string]any{"message": tc.message}}, now)
			var upstream *upstreamError
			if !errors.As(err, &upstream) || upstream.QuotaType != tc.kind || upstream.Local {
				t.Fatalf("wrong quota classification: %v", err)
			}
			if tc.kind != "" && (upstream.Kind != "upstream_quota_exhausted" || upstream.Scope != "account") {
				t.Fatal("quota must have account scope")
			}
			if tc.kind == "weekly" && (upstream.QuotaResetAt == nil || upstream.QuotaResetAt.Sub(now) != 58*time.Hour) {
				t.Fatal("multi-day reset was lost")
			}
		})
	}
}

func TestQuotaFailedPersistenceRetainsBlockAndFreshRecovery(t *testing.T) {
	s := NewService()
	s.SetHost(func(method string, _, _ any) error {
		t.Errorf("quota unexpectedly called %s", method)
		return fmt.Errorf("unexpected host callback")
	})
	one := Credential{ID: "one", APIKey: "fixture-one"}
	two := Credential{ID: "two", APIKey: "fixture-two", Disabled: true}
	for _, c := range []*Credential{&one, &two} {
		c.QuotaIdentity = &QuotaIdentity{AccountHash: "verified-account", KeyHash: quotaKeyHash(*c)}
	}
	old := quotaTestAuth(t, s, one)
	quotaTestAuth(t, s, two)
	quotaTestAuth(t, s, Credential{ID: "unknown", APIKey: one.APIKey})
	path := filepath.Join(s.authDir, "one.json")
	if err := os.Rename(path, path+".bak"); err != nil {
		t.Fatal(err)
	}
	reset := time.Now().Add(-time.Minute)
	quotaErr := &upstreamError{APIError: &APIError{429, "upstream_quota_exhausted", "quota exhausted"}, QuotaType: "weekly", QuotaResetAt: &reset}
	if err := s.observeQuotaError(one, quotaErr); err == nil {
		t.Fatal("missing file did not produce a visible persistence error")
	}
	status := s.quotaStatus(one.ID)
	if status["persistence_error"] == "" || status["quota_paused"] != true || s.creds[two.ID].QuotaBlock == nil || s.creds["unknown"].QuotaBlock != nil {
		t.Fatal("failed persistence lost a block or merged unknown identity")
	}
	block := status["quota_block"].(map[string]any)
	if len(block) != 4 || block["key_hash"] != nil || block["account_hash"] != nil {
		t.Fatal("public quota block exposes internal fields")
	}
	_, err := s.selectedCredential(ExecutorRequest{StorageJSON: jsonBytes(one)})
	var local *upstreamError
	if !errors.As(err, &local) || !local.Local || local.Scope != "account" || !local.RetryAt.Equal(reset) {
		t.Fatal("local last defense is missing structured metadata")
	}
	if err := os.Rename(path+".bak", path); err != nil {
		t.Fatal(err)
	}
	if quotaTestParse(t, s, one.ID, old)["Disabled"] != true || s.creds[one.ID].QuotaBlock == nil {
		t.Fatal("old watcher read erased the in-memory block")
	}
	if err := s.reconcileQuotaScheduling(); err != nil {
		t.Fatal(err)
	}
	if s.quotaStatus(one.ID)["persistence_error"] != "" {
		t.Fatal("successful retry left a persistence error")
	}
	reloaded := NewService()
	if quotaTestParse(t, reloaded, one.ID, quotaTestRead(t, s, one.ID))["Disabled"] != true {
		t.Fatal("persisted quota did not disable CPA auth after reload")
	}
	observed := s.creds[one.ID].QuotaBlock.ObservedAt
	room := quotaTestRoom("verified-account", observed.Add(time.Second))
	if err := s.observeQuotaUsage(s.creds[one.ID], room, observed.Add(-time.Second)); err != nil || s.creds[one.ID].QuotaBlock == nil {
		t.Fatal("an old in-flight success cleared a newer block")
	}
	failed := room
	failed.Status = "rate_limited"
	if err := s.observeQuotaUsage(s.creds[one.ID], failed, observed.Add(time.Second)); err != nil || s.creds[one.ID].QuotaBlock == nil {
		t.Fatal("failed sampling cleared the block")
	}
	if err := os.Rename(path, path+".bak"); err != nil {
		t.Fatal(err)
	}
	if err := s.observeQuotaUsage(s.creds[one.ID], room, observed.Add(time.Second)); err == nil || s.creds[one.ID].QuotaBlock == nil {
		t.Fatal("failed recovery persistence enabled the credential")
	}
	if err := os.Rename(path+".bak", path); err != nil {
		t.Fatal(err)
	}
	if err := s.observeQuotaUsage(s.creds[one.ID], room, observed.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if s.creds[one.ID].QuotaBlock != nil || s.creds[two.ID].QuotaBlock != nil || !s.creds[two.ID].Disabled {
		t.Fatal("account recovery changed manual Disabled")
	}
	for _, c := range []Credential{one, two} {
		raw := quotaTestRead(t, s, c.ID)
		if !bytes.Contains(raw, []byte("9007199254740993")) || bytes.Contains(raw, []byte(`"quota_block"`)) {
			t.Fatal("recovery lost unknown JSON or persisted a stale block")
		}
		if quotaTestParse(t, NewService(), c.ID, raw)["Disabled"] != c.Disabled {
			t.Fatal("CPA effective Disabled does not match recovered/manual state")
		}
	}
}

func TestQuotaToggleAndKeyRotation(t *testing.T) {
	s := NewService()
	c := Credential{ID: "one", APIKey: "fixture-old", RoutingID: "rotated-routing-id"}
	quotaTestAuth(t, s, c)
	quotaErr := &upstreamError{APIError: &APIError{429, "upstream_quota_exhausted", "quota exhausted"}, QuotaType: "weekly"}
	if err := s.observeQuotaError(c, quotaErr); err != nil {
		t.Fatal(err)
	}
	before := s.creds[c.ID].QuotaRevision
	enabled := false
	s.cfg.QuotaAutoPause = &enabled
	if err := s.reconcileQuotaScheduling(); err != nil {
		t.Fatal(err)
	}
	if s.creds[c.ID].QuotaRevision == before || quotaTestParse(t, s, c.ID, quotaTestRead(t, s, c.ID))["Disabled"] != false {
		t.Fatal("toggle did not rewrite watcher state")
	}
	enabled = true
	if err := s.reconcileQuotaScheduling(); err != nil {
		t.Fatal(err)
	}
	if quotaTestParse(t, s, c.ID, quotaTestRead(t, s, c.ID))["Disabled"] != true {
		t.Fatal("re-enable did not pause CPA auth")
	}
	next := s.creds[c.ID]
	next.APIKey = "fixture-replaced"
	if err := s.saveCredential(next); err != nil {
		t.Fatal(err)
	}
	if s.creds[c.ID].QuotaBlock != nil || s.creds[c.ID].QuotaIdentity != nil {
		t.Fatal("key replacement inherited old quota identity")
	}
	if err := s.observeQuotaError(c, quotaErr); err != nil || s.creds[c.ID].QuotaBlock != nil {
		t.Fatal("old key's late quota error blocked the replacement")
	}
	raw := quotaTestRead(t, s, c.ID)
	if bytes.Contains(raw, []byte(`"quota_block"`)) || !bytes.Contains(raw, []byte("9007199254740993")) {
		t.Fatal("rotation failed to clean persisted block/preserve metadata")
	}
}

func TestQuotaUsage100StopsCPAAndRemainsSamplingTarget(t *testing.T) {
	s := usageTestService(t, func(req map[string]any) (int, string) {
		if strings.HasSuffix(str(req["url"]), "/users/me/plan") {
			return 200, testPlan
		}
		return 200, `{"success":true,"data":{"limits":[{"type":"weekly","percentUsed":100}]}}`
	})
	quotaTestAuth(t, s, s.creds["one"])
	value, err := s.credentialUsage(usageTestRequest())
	if err != nil || value.Status != "ok" || value.accountHash == "" || s.creds["one"].QuotaBlock == nil {
		t.Fatalf("100 percent sample did not block: %v", err)
	}
	if quotaTestParse(t, s, "one", quotaTestRead(t, s, "one"))["Disabled"] != true {
		t.Fatal("100 percent sample did not disable CPA auth")
	}
	targets := s.usageSamplingTargets(time.Now().Add(2 * usageTTL))
	if len(targets) != 1 || targets[0] != "one" || s.creds["one"].Disabled {
		t.Fatal("auto-pause stopped future usage sampling or changed manual Disabled")
	}
}
