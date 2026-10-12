package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func errorRulesRequest(t *testing.T, s *Service, method, path string, body []byte, status int) ManagementResponse {
	t.Helper()
	out, err := s.management(jsonBytes(ManagementRequest{Method: method, Path: apiBase + path, Body: body}))
	if err != nil {
		t.Fatal(err)
	}
	response := out.(ManagementResponse)
	if response.StatusCode != status {
		t.Fatalf("%s %s status=%d, want %d: %s", method, path, response.StatusCode, status, response.Body)
	}
	return response
}

func errorRulesResult(t *testing.T, response ManagementResponse) errorRulesResponse {
	t.Helper()
	var result errorRulesResponse
	if err := json.Unmarshal(response.Body, &result); err != nil {
		t.Fatal(err)
	}
	if result.Rules == nil || result.Defaults == nil || result.Sync.Failed == nil {
		t.Fatal("API collections must serialize as arrays")
	}
	return result
}

func errorRulesFixtureRules() []RequestErrorRule {
	return []RequestErrorRule{
		{Status: 100, Match: []string{"empty response content"}, Action: "stop"},
		{Status: 429, MatchRegexr: []string{`(?i)quota\s+exhausted`}, Action: "stop-and-cooldown"},
		{Status: 500, Match: []string{"retry"}, MatchRegexr: []string{`^upstream`}, Action: "continue"},
		{Status: 599, Match: []string{"busy"}, Action: "continue-and-cooldown"},
	}
}

func TestErrorRulesDefaultsValidationAndIsolation(t *testing.T) {
	s := registeredService(t, "")
	defaults := errorRulesResult(t, errorRulesRequest(t, s, "GET", "/error-rules", nil, 200))
	if len(defaults.Rules) != 5 || !reflect.DeepEqual(defaults.Rules, defaults.Defaults) || defaults.Sync.Total != 0 {
		t.Fatal("fresh install changed the five defaults")
	}
	rules := errorRulesFixtureRules()
	result := errorRulesResult(t, errorRulesRequest(t, s, "PUT", "/error-rules", jsonBytes(errorRulesDocument{rules}), 200))
	if !reflect.DeepEqual(result.Rules, rules) || bytes.Contains(jsonBytes(result.Rules), []byte("match_regexr")) {
		t.Fatal("CPA matcher schema or rule ordering changed")
	}
	before, err := os.ReadFile(filepath.Join(s.config().DataDir, errorRulesFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{
		`null`, `[]`, `{}`, `{"rules":null}`, `{"rules":{}}`, `{"rules":[null]}`,
		`{"rules":[],"extra":true}`, `{"rules":[],"rules":[]}`, `{"rules":[]} {}`,
		`{"rules":[{"status":99,"match":["x"],"action":"stop"}]}`,
		`{"rules":[{"status":600,"match":["x"],"action":"stop"}]}`,
		`{"rules":[{"status":429.5,"match":["x"],"action":"stop"}]}`,
		`{"rules":[{"status":"429","match":["x"],"action":"stop"}]}`,
		`{"rules":[{"status":429,"match":["x"],"action":"invalid"}]}`,
		`{"rules":[{"status":429,"match":[],"action":"stop"}]}`,
		`{"rules":[{"status":429,"match":["   "],"action":"stop"}]}`,
		`{"rules":[{"status":429,"match":[null],"action":"stop"}]}`,
		`{"rules":[{"status":429,"match":null,"match-regexr":["x"],"action":"stop"}]}`,
		`{"rules":[{"status":429,"match-regexr":["[private-fixture-key"],"action":"stop"}]}`,
		`{"rules":[{"status":429,"match_regexr":["x"],"action":"stop"}]}`,
		`{"rules":[{"status":429,"status":500,"match":["x"],"action":"stop"}]}`,
	} {
		response := errorRulesRequest(t, s, "PUT", "/error-rules", []byte(invalid), 400)
		if bytes.Contains(response.Body, []byte("private-fixture-key")) {
			t.Fatal("validation error reflected supplied secrets")
		}
		if !reflect.DeepEqual(s.currentErrorRules(), rules) {
			t.Fatal("rejected update mutated live rules")
		}
	}
	for _, path := range []string{"/error-rules/sync", "/error-rules/reset"} {
		errorRulesRequest(t, s, "POST", path, []byte(`{"rules":[]}`), 400)
	}
	after, err := os.ReadFile(filepath.Join(s.config().DataDir, errorRulesFile))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("rejected update changed persisted policy")
	}
	copy := s.currentErrorRules()
	copy[0].Match[0], copy[1].MatchRegexr[0] = "mutated", "mutated"
	if !reflect.DeepEqual(s.currentErrorRules(), rules) {
		t.Fatal("policy snapshot shares mutable matcher slices")
	}
}

func TestErrorRulesPhysicalSyncFailureRetryAndFieldPreservation(t *testing.T) {
	f := newAffinityFixture(t)
	// Include raw quota data and a rotated identity. A policy edit must never
	// normalize stale quota or replace the operator's physical disabled flag.
	fields := configFidelityCredentialFields(t, f.original["migrated-b.json"])
	fields["quota_block"] = json.RawMessage(`{"key_hash":"old-key","source":"usage","limits":[{"type":"weekly"}],"future":9007199254740993}`)
	fields["quota_revision"] = jsonBytes("keep-quota-revision")
	fields["routing_id"] = jsonBytes("rotated-fixture-id")
	before := jsonBytes(fields)
	path := filepath.Join(f.s.authDir, "migrated-b.json")
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.watch(); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(f.s.authDir, "migrated-a.json")
	if err := os.Rename(missing, missing+".bak"); err != nil {
		t.Fatal(err)
	}
	rules := errorRulesFixtureRules()
	result := errorRulesResult(t, errorRulesRequest(t, f.s, "PUT", "/error-rules", jsonBytes(errorRulesDocument{rules}), 200))
	if result.Sync.Total != 2 || result.Sync.Succeeded != 1 || len(result.Sync.Failed) != 1 || result.Sync.Failed[0].ID != "stable-0" {
		t.Fatalf("partial sync not reported: %+v", result.Sync)
	}
	for _, c := range f.s.creds {
		if bytes.Contains(jsonBytes(result.Sync), []byte(c.APIKey)) {
			t.Fatal("sync error leaked a key")
		}
	}
	savedRaw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	assertAffinityFields(t, before, savedRaw, "request_scoped_errors")
	if err := os.Rename(missing+".bak", missing); err != nil {
		t.Fatal(err)
	}
	result = errorRulesResult(t, errorRulesRequest(t, f.s, "POST", "/error-rules/sync", []byte(`{}`), 200))
	if result.Sync.Succeeded != 2 || len(result.Sync.Failed) != 0 {
		t.Fatalf("retry did not synchronize every account: %+v", result.Sync)
	}
	for name, raw := range f.files() {
		var credential Credential
		if err := json.Unmarshal(raw, &credential); err != nil || !reflect.DeepEqual(credential.RequestScopedErrors, rules) {
			t.Fatalf("file %s retained old rules", name)
		}
	}
	info, _ := os.Stat(path)
	errorRulesRequest(t, f.s, "POST", "/error-rules/sync", []byte(`{}`), 200)
	next, _ := os.Stat(path)
	if !info.ModTime().Equal(next.ModTime()) {
		t.Fatal("idempotent sync rewrote an unchanged credential")
	}
	result = errorRulesResult(t, errorRulesRequest(t, f.s, "POST", "/error-rules/reset", []byte(`{}`), 200))
	if len(result.Rules) != 5 || !reflect.DeepEqual(result.Rules, result.Defaults) || result.Sync.Succeeded != 2 {
		t.Fatal("reset did not persist and synchronize defaults")
	}
}

func TestErrorRulesHostFallbackPreservesUnknownAndRedactsFailure(t *testing.T) {
	s := registeredService(t, "")
	const key = "private-fixture-key"
	raw := []byte(`{"type":"cline-pass","id":"account","api_key":"private-fixture-key","disabled":true,"unknown":{"serial":9007199254740993},"quota_revision":"keep"}`)
	if _, err := s.parseAuth(jsonBytes(map[string]any{"FileName": "account.json", "RawJSON": raw})); err != nil {
		t.Fatal(err)
	}
	var saved []byte
	failSave := true
	s.SetHost(func(method string, in, out any) error {
		if method != "host.auth.save" {
			return fmt.Errorf("unexpected callback %s", method)
		}
		if failSave {
			return fmt.Errorf("host error includes Authorization: Bearer %s and %s", key, raw)
		}
		saved = append([]byte{}, in.(map[string]any)["json"].(json.RawMessage)...)
		// Synchronous host reparsing also exercises callback lock ordering.
		_, err := s.parseAuth(jsonBytes(map[string]any{"FileName": "account.json", "RawJSON": saved}))
		return err
	})
	rules := errorRulesFixtureRules()
	response := errorRulesRequest(t, s, "PUT", "/error-rules", jsonBytes(errorRulesDocument{rules}), 200)
	result := errorRulesResult(t, response)
	if result.Sync.Succeeded != 0 || len(result.Sync.Failed) != 1 || bytes.Contains(response.Body, []byte(key)) {
		t.Fatal("host failure lost sync state or leaked its credential")
	}
	failSave = false
	result = errorRulesResult(t, errorRulesRequest(t, s, "POST", "/error-rules/sync", []byte(`{}`), 200))
	if result.Sync.Succeeded != 1 || len(result.Sync.Failed) != 0 {
		t.Fatal("host fallback retry failed")
	}
	fields := configFidelityCredentialFields(t, saved)
	if string(fields["unknown"]) != `{"serial":9007199254740993}` || string(fields["disabled"]) != "true" || string(fields["quota_revision"]) != `"keep"` {
		t.Fatal("host fallback dropped unknown numbers or manual/quota fields")
	}
}

func TestErrorRulesRestartParseRefreshNewAccountAndEmpty(t *testing.T) {
	for _, rules := range [][]RequestErrorRule{errorRulesFixtureRules(), {}} {
		t.Run(fmt.Sprintf("rules-%d", len(rules)), func(t *testing.T) {
			s := registeredService(t, "")
			errorRulesRequest(t, s, "PUT", "/error-rules", jsonBytes(errorRulesDocument{rules}), 200)
			restarted := NewService()
			config := fmt.Sprintf("data_dir: %q\n", filepath.ToSlash(s.config().DataDir))
			if err := restarted.configure(jsonBytes(map[string]any{"config_yaml": []byte(config)})); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(restarted.currentErrorRules(), rules) {
				t.Fatal("restart restored obsolete defaults")
			}
			stale := Credential{Type: Provider, ID: "old", APIKey: "fixture-existing-key", Disabled: true, RequestScopedErrors: requestErrorRules()}
			raw := quotaTestAuth(t, restarted, stale)
			auth := quotaTestParse(t, restarted, stale.ID, raw)
			assertErrorRulesAuth(t, auth, rules)
			var disk Credential
			_ = json.Unmarshal(quotaTestRead(t, restarted, stale.ID), &disk)
			if !reflect.DeepEqual(disk.RequestScopedErrors, rules) || !disk.Disabled {
				t.Fatal("restart parser did not persist policy while preserving manual disable")
			}
			refreshed, err := restarted.refreshAuth(jsonBytes(map[string]any{"StorageJSON": jsonBytes(stale)}))
			if err != nil {
				t.Fatal(err)
			}
			assertErrorRulesAuth(t, refreshed.(map[string]any)["Auth"].(map[string]any), rules)
			restarted.SetHost(func(method string, in, out any) error {
				if method != "host.auth.save" {
					return fmt.Errorf("unexpected callback %s", method)
				}
				request := in.(map[string]any)
				name := request["name"].(string)
				raw := request["json"].(json.RawMessage)
				path := filepath.Join(restarted.authDir, name)
				if err := os.WriteFile(path, raw, 0600); err != nil {
					return err
				}
				if _, err := restarted.parseAuth(jsonBytes(map[string]any{"FileName": name, "RawJSON": []byte(raw), "Host": map[string]any{"AuthDir": restarted.authDir}})); err != nil {
					return err
				}
				return json.Unmarshal(jsonBytes(map[string]any{"path": path}), out)
			})
			response := errorRulesRequest(t, restarted, "POST", "/credentials", []byte(`{"label":"new","api_key":"fixture-new-key"}`), 201)
			var imported struct{ ID string }
			_ = json.Unmarshal(response.Body, &imported)
			if !reflect.DeepEqual(restarted.creds[imported.ID].RequestScopedErrors, rules) {
				t.Fatal("new account restored defaults")
			}
			result := errorRulesResult(t, errorRulesRequest(t, restarted, "GET", "/error-rules", nil, 200))
			if result.Inherits != (len(rules) == 0) || result.Sync.Succeeded != 2 {
				t.Fatal("empty-policy inheritance or restart sync state changed")
			}
		})
	}
}

func assertErrorRulesAuth(t *testing.T, auth map[string]any, rules []RequestErrorRule) {
	t.Helper()
	var storage Credential
	if err := json.Unmarshal(auth["StorageJSON"].([]byte), &storage); err != nil || !reflect.DeepEqual(storage.RequestScopedErrors, rules) {
		t.Fatal("runtime storage uses stale rules")
	}
	metadata := auth["Metadata"].(map[string]any)["request_scoped_errors"]
	var metadataRules []RequestErrorRule
	if err := json.Unmarshal(jsonBytes(metadata), &metadataRules); err != nil || !reflect.DeepEqual(metadataRules, rules) {
		t.Fatal("runtime metadata and storage use different rules")
	}
}

func TestErrorRulesSettingsAndLateSamplerPreserveCurrentPolicy(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s := usageTestService(t, func(req map[string]any) (int, string) {
		if strings.HasSuffix(str(req["url"]), "/usage-limits") {
			once.Do(func() { close(started); <-release })
			return 200, `{"success":true,"data":{"limits":[{"type":"weekly","percentUsed":100}]}}`
		}
		return 200, testPlan
	})
	c := s.creds["one"]
	c.Disabled = true
	before := quotaTestAuth(t, s, c)
	done := make(chan error, 1)
	go func() { _, err := s.credentialUsage(usageTestRequest()); done <- err }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("sampler did not start")
	}
	rules := errorRulesFixtureRules()
	errorRulesRequest(t, s, "PUT", "/error-rules", jsonBytes(errorRulesDocument{rules}), 200)
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("late sampler deadlocked")
	}
	after := quotaTestRead(t, s, c.ID)
	assertAffinityFields(t, before, after, "request_scoped_errors", "quota_block", "quota_identity", "quota_revision")
	var saved Credential
	_ = json.Unmarshal(after, &saved)
	if !saved.Disabled || saved.QuotaBlock == nil || !reflect.DeepEqual(saved.RequestScopedErrors, rules) {
		t.Fatal("late sampler wrote obsolete policy or changed manual disable")
	}
	policyBefore, _ := os.ReadFile(filepath.Join(s.config().DataDir, errorRulesFile))
	errorRulesRequest(t, s, "PUT", "/config", []byte(`{"timeout_seconds":120,"error_rules":[],"request_scoped_errors":[]}`), 200)
	errorRulesRequest(t, s, "PUT", "/models", jsonBytes(map[string]any{"models": s.config().Models}), 200)
	policyAfter, _ := os.ReadFile(filepath.Join(s.config().DataDir, errorRulesFile))
	if !bytes.Equal(policyBefore, policyAfter) || !reflect.DeepEqual(s.currentErrorRules(), rules) {
		t.Fatal("settings/model save overwrote the unified policy")
	}
	_ = json.Unmarshal(quotaTestRead(t, s, c.ID), &saved)
	if !reflect.DeepEqual(saved.RequestScopedErrors, rules) {
		t.Fatal("registration refresh restored defaults")
	}
	result, err := s.updateCredential(ManagementRequest{Query: url.Values{"id": {c.ID}}, Body: []byte(`{"label":"renamed"}`)})
	if err != nil || result.(ManagementResponse).StatusCode != 200 {
		t.Fatal("credential edit failed")
	}
	_ = json.Unmarshal(quotaTestRead(t, s, c.ID), &saved)
	if !reflect.DeepEqual(saved.RequestScopedErrors, rules) {
		t.Fatal("credential edit restored defaults")
	}
}

func TestErrorRulesConcurrentSavesMatchPersistence(t *testing.T) {
	f := newAffinityFixture(t)
	start := make(chan struct{})
	results := make(chan ManagementResponse, 2)
	for _, rules := range [][]RequestErrorRule{errorRulesFixtureRules(), {}} {
		go func(rules []RequestErrorRule) {
			<-start
			out, err := f.s.management(jsonBytes(ManagementRequest{Method: "PUT", Path: apiBase + "/error-rules", Body: jsonBytes(errorRulesDocument{rules})}))
			if err != nil {
				t.Error(err)
				results <- ManagementResponse{}
				return
			}
			results <- out.(ManagementResponse)
		}(rules)
	}
	close(start)
	for i := 0; i < 2; i++ {
		select {
		case result := <-results:
			if result.StatusCode != 200 {
				t.Fatal("concurrent save failed")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("concurrent save deadlocked")
		}
	}
	persisted, err := loadErrorRules(f.s.config().DataDir)
	if err != nil || !reflect.DeepEqual(persisted, f.s.currentErrorRules()) {
		t.Fatal("late save left a stale policy snapshot")
	}
	for _, raw := range f.files() {
		var credential Credential
		_ = json.Unmarshal(raw, &credential)
		if !reflect.DeepEqual(credential.RequestScopedErrors, persisted) {
			t.Fatal("late save left stale credential rules")
		}
	}
}

func TestErrorRulesPersistenceFailureAndInvalidRestart(t *testing.T) {
	s := registeredService(t, "")
	path := filepath.Join(s.config().DataDir, errorRulesFile)
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	errorRulesRequest(t, s, "PUT", "/error-rules", jsonBytes(errorRulesDocument{errorRulesFixtureRules()}), 500)
	if !reflect.DeepEqual(s.currentErrorRules(), requestErrorRules()) {
		t.Fatal("failed persistence published unsaved rules")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"rules":[{"status":429,"match-regexr":["[private-fixture-key"],"action":"stop"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := loadErrorRules(s.config().DataDir)
	if err == nil || strings.Contains(err.Error(), "private-fixture-key") {
		t.Fatal("invalid persisted regex was accepted or leaked")
	}
}

func TestErrorRulesRoutesAndEmbeddedModule(t *testing.T) {
	s := registeredService(t, "")
	registration, err := s.registerManagement(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Handle("plugin.shutdown", nil)
	found := map[string]bool{}
	for _, route := range registration.(map[string]any)["routes"].([]map[string]string) {
		found[route["Method"]+" "+route["Path"]] = true
	}
	for _, route := range []string{"GET /error-rules", "PUT /error-rules", "POST /error-rules/sync", "POST /error-rules/reset"} {
		parts := strings.SplitN(route, " ", 2)
		if !found[parts[0]+" "+apiBase+parts[1]] {
			t.Fatalf("missing route %s", route)
		}
	}
	out, err := s.management(jsonBytes(ManagementRequest{Method: "GET", Path: "/v0/resource/plugins/" + PluginID + "/console"}))
	if err != nil {
		t.Fatal(err)
	}
	response := out.(ManagementResponse)
	module, err := ui.ReadFile("ui/error-rules.js")
	if err != nil || response.StatusCode != 200 || bytes.Contains(response.Body, []byte("/*__ERROR_RULES_MODULE__*/")) || !bytes.Contains(response.Body, module) {
		t.Fatal("management page did not inject the error-rules module")
	}
}
