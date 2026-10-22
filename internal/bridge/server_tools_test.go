package bridge

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
)

// serverToolsService registers a bridge whose settings carry the given
// server_tools YAML block (indented two spaces under the key).
func serverToolsService(t *testing.T, rules string) *Service {
	t.Helper()
	s := NewService()
	configYAML := fmt.Sprintf("data_dir: %q\n", filepath.ToSlash(t.TempDir()))
	configYAML += "models:\n  - id: deepseek-flash\n    upstream_id: cline-pass/deepseek-v4.1-flash\n"
	configYAML += "nonstream_mode: native\n"
	configYAML += "server_tools:\n" + rules
	if _, err := s.Handle("plugin.register", jsonBytes(map[string]any{"config_yaml": []byte(configYAML)})); err != nil {
		t.Fatalf("register plugin: %v", err)
	}
	return s
}

func serverToolsPlan() hostPlan {
	return jsonPlan(map[string]any{"choices": []any{map[string]any{
		"index":         0,
		"message":       map[string]any{"role": "assistant", "content": "ok"},
		"finish_reason": "stop",
	}}})
}

// outboundBody runs one non-streaming request and returns the upstream body.
func outboundBody(t *testing.T, s *Service, payload string) map[string]any {
	t.Helper()
	_, bodies := reasoningMappingHost(t, s, serverToolsPlan())
	var req ExecutorRequest
	if err := json.Unmarshal(executorRequest("server-tools"), &req); err != nil {
		t.Fatal(err)
	}
	req.Payload = []byte(payload)
	if _, err := s.Handle("executor.execute", jsonBytes(req)); err != nil {
		t.Fatal(err)
	}
	select {
	case body := <-bodies:
		return body
	default:
		t.Fatal("no upstream request was made")
		return nil
	}
}

func toolTypes(body map[string]any) []string {
	var out []string
	for _, entry := range list(body["tools"]) {
		tool := object(entry)
		out = append(out, str(tool["type"]))
	}
	return out
}

func TestServerToolsAreAppendedToMatchingRequests(t *testing.T) {
	rules := "  - models: [\"deepseek-v4.1-flash-*\", \"deepseek-flash\"]\n" +
		"    tools:\n      - type: vercel:exa_search\n      - type: vercel:tako_search\n"
	s := serverToolsService(t, rules)
	body := outboundBody(t, s, `{"messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`)
	got := toolTypes(body)
	want := []string{"function", "vercel:exa_search", "vercel:tako_search"}
	if len(got) != len(want) {
		t.Fatalf("tools = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tools = %v, want %v", got, want)
		}
	}
	// The caller's own tool definition must survive the append untouched.
	if name := str(object(object(list(body["tools"])[0])["function"])["name"]); name != "lookup" {
		t.Fatalf("client tool was mutated: %v", body["tools"])
	}
}

func TestServerToolsCreateTheToolsArrayWhenAbsent(t *testing.T) {
	rules := "  - models: [\"deepseek-flash\"]\n    tools:\n      - type: vercel:exa_search\n"
	s := serverToolsService(t, rules)
	body := outboundBody(t, s, `{"messages":[{"role":"user","content":"hello"}]}`)
	if got := toolTypes(body); len(got) != 1 || got[0] != "vercel:exa_search" {
		t.Fatalf("tools = %v, want [vercel:exa_search]", got)
	}
}

func TestServerToolsSkipDuplicatesAndOtherModels(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pattern string
		payload string
		want    int
	}{
		{
			name:    "caller already declared the same tool",
			pattern: "deepseek-flash",
			payload: `{"messages":[{"role":"user","content":"hello"}],"tools":[{"type":"vercel:exa_search"}]}`,
			want:    1,
		},
		{
			name:    "rule targets another model family",
			pattern: "deepseek-v9-*",
			payload: `{"messages":[{"role":"user","content":"hello"}]}`,
			want:    0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rules := fmt.Sprintf("  - models: [%q]\n    tools:\n      - type: vercel:exa_search\n", tc.pattern)
			s := serverToolsService(t, rules)
			if got := toolTypes(outboundBody(t, s, tc.payload)); len(got) != tc.want {
				t.Fatalf("tools = %v, want %d entries", got, tc.want)
			}
		})
	}
}

func TestServerToolsRejectInvalidRules(t *testing.T) {
	for _, tc := range []struct{ name, rules string }{
		{"no models", "  - tools:\n      - type: vercel:exa_search\n"},
		{"no tools", "  - models: [\"deepseek-flash\"]\n    tools: []\n"},
		{"tool without type", "  - models: [\"deepseek-flash\"]\n    tools:\n      - name: x\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewService()
			configYAML := fmt.Sprintf("data_dir: %q\n", filepath.ToSlash(t.TempDir()))
			configYAML += "models:\n  - id: deepseek-flash\n    upstream_id: cline-pass/deepseek-v4.1-flash\n"
			configYAML += "server_tools:\n" + tc.rules
			if _, err := s.Handle("plugin.register", jsonBytes(map[string]any{"config_yaml": []byte(configYAML)})); err == nil {
				t.Fatal("expected the settings to be rejected")
			}
		})
	}
}

func TestModelPatternMatches(t *testing.T) {
	for _, tc := range []struct {
		pattern, model string
		want           bool
	}{
		{"deepseek-v4.1-flash-*", "deepseek-v4.1-flash-1017", true},
		{"deepseek-v4.1-flash-*", "deepseek-v4-flash", false},
		{"*-flash", "deepseek-flash", true},
		{"*", "anything", true},
		{"exact", "exact", true},
		{"exact", "exactly", false},
		{"", "anything", false},
	} {
		if got := modelPatternMatches(tc.pattern, tc.model); got != tc.want {
			t.Errorf("modelPatternMatches(%q, %q) = %v, want %v", tc.pattern, tc.model, got, tc.want)
		}
	}
}

func modelServerToolsCall(t *testing.T, s *Service, method, body string) (int, map[string]any) {
	t.Helper()
	out, err := s.management(jsonBytes(map[string]any{
		"method": method,
		"path":   "/v0/management/clinepassbridge/models/server-tools",
		"body":   []byte(body),
	}))
	if err != nil {
		t.Fatalf("management %s: %v", method, err)
	}
	resp, ok := out.(ManagementResponse)
	if !ok {
		t.Fatalf("management %s returned %T", method, out)
	}
	var payload map[string]any
	if len(resp.Body) > 0 {
		if err := json.Unmarshal(resp.Body, &payload); err != nil {
			t.Fatalf("decode %s body: %v", method, err)
		}
	}
	return resp.StatusCode, payload
}

func TestServerToolsManagementRoundTrip(t *testing.T) {
	s := serverToolsService(t, "  - models: [\"deepseek-v4.1-flash-*\"]\n    tools:\n      - type: vercel:exa_search\n")

	status, payload := modelServerToolsCall(t, s, "GET", "")
	if status != 200 {
		t.Fatalf("GET status = %d, want 200", status)
	}
	models, _ := payload["models"].(map[string]any)
	if got, _ := models["deepseek-flash"].([]any); len(got) != 0 {
		t.Fatalf("wildcard rule must not leak into deepseek-flash, got %v", got)
	}
	if catalog, _ := payload["catalog"].([]any); len(catalog) == 0 {
		t.Fatal("GET did not return the tool catalog")
	}

	// Duplicate and blank type names collapse; the model keeps an exact rule.
	status, payload = modelServerToolsCall(t, s, "PUT", `{"model":"deepseek-flash","tools":["vercel:exa_search"," vercel:exa_search ","","vercel:tako_search"]}`)
	if status != 200 {
		t.Fatalf("PUT status = %d body = %v", status, payload)
	}
	models, _ = payload["models"].(map[string]any)
	if got, _ := models["deepseek-flash"].([]any); len(got) != 2 {
		t.Fatalf("models[deepseek-flash] = %v, want two tools", models["deepseek-flash"])
	}
	body := outboundBody(t, s, `{"messages":[{"role":"user","content":"hello"}]}`)
	if got := toolTypes(body); len(got) != 2 || got[0] != "vercel:exa_search" || got[1] != "vercel:tako_search" {
		t.Fatalf("outbound tools = %v, want the saved server tools", got)
	}

	// Clearing the list removes the rule again.
	if status, _ := modelServerToolsCall(t, s, "PUT", `{"model":"deepseek-flash","tools":[]}`); status != 200 {
		t.Fatalf("clearing status = %d, want 200", status)
	}
	if rules := s.config().ServerTools; len(rules) != 0 {
		t.Fatalf("clearing left %d rules, want none", len(rules))
	}
	if got := toolTypes(outboundBody(t, s, `{"messages":[{"role":"user","content":"hello"}]}`)); len(got) != 0 {
		t.Fatalf("outbound tools = %v, want none after clearing", got)
	}

	if status, _ := modelServerToolsCall(t, s, "PUT", `{"model":"nope","tools":["vercel:exa_search"]}`); status != 404 {
		t.Fatalf("unknown model status = %d, want 404", status)
	}
	if status, _ := modelServerToolsCall(t, s, "PUT", "{oops}"); status != 400 {
		t.Fatalf("invalid JSON status = %d, want 400", status)
	}
}
