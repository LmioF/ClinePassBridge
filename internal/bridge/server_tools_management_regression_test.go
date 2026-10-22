package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func serverToolsManagementRegistration(dir, rules string) []byte {
	yaml := fmt.Sprintf("data_dir: %q\nmodels:\n  - id: alpha\n    upstream_id: cline-pass/alpha\n  - id: beta\n    upstream_id: cline-pass/beta\nserver_tools:\n%s", filepath.ToSlash(dir), rules)
	return jsonBytes(map[string]any{"config_yaml": []byte(yaml)})
}

func registerServerToolsManagement(t *testing.T, raw []byte) *Service {
	t.Helper()
	s := NewService()
	if _, err := s.Handle("plugin.register", raw); err != nil {
		t.Fatal(err)
	}
	return s
}

func preparedServerTools(t *testing.T, s *Service, model string) []byte {
	t.Helper()
	body, _, _, err := s.prepareWithCredential(ExecutorRequest{
		Model: model, Payload: []byte(`{"messages":[{"role":"user","content":"hello"}]}`),
	}, Credential{})
	if err != nil {
		t.Fatal(err)
	}
	return jsonBytes(body["tools"])
}

func TestServerToolsManagementPreservesCompleteToolsThroughReload(t *testing.T) {
	raw := serverToolsManagementRegistration(t.TempDir(), `  - models: ["*"]
    tools:
      - type: vercel:exa_search
        name: docs
        config:
          num_results: 1
          include_domains: [docs.example]
      - type: function
        function:
          name: lookup
          parameters:
            type: object
            properties:
              id: {type: integer, const: 7}
      - type: vercel:exa_search
        name: news
        config: {num_results: 2}
      - type: function
        function:
          name: fetch
          parameters: {type: object}
      - type: vercel:browserbase_fetch
        config: {mode: raw}
  - models: [alpha]
    tools:
      - type: function
        function: {name: lookup, description: shadowed}
`)
	s := registerServerToolsManagement(t, raw)
	alphaBefore := preparedServerTools(t, s, "alpha")
	var betaWant []map[string]any
	if err := json.Unmarshal(preparedServerTools(t, s, "beta"), &betaWant); err != nil {
		t.Fatal(err)
	}
	// Keep both named searches and both named functions, remove fetch, add Tako.
	betaWant = append(betaWant[:len(betaWant)-1], map[string]any{"type": "vercel:tako_search"})
	if status, payload := modelServerToolsCall(t, s, "PUT", `{"model":"beta","tools":["vercel:exa_search","function","vercel:tako_search"]}`); status != 200 {
		t.Fatalf("save beta: status=%d body=%v", status, payload)
	}
	for _, current := range []*Service{s, registerServerToolsManagement(t, raw)} {
		if got := preparedServerTools(t, current, "alpha"); !bytes.Equal(got, alphaBefore) {
			t.Fatalf("saving beta changed alpha's tools: got %s, want %s", got, alphaBefore)
		}
		if got, want := preparedServerTools(t, current, "beta"), jsonBytes(betaWant); !bytes.Equal(got, want) {
			t.Fatalf("saving beta lost retained definitions: got %s, want %s", got, want)
		}
		for _, rule := range current.config().ServerTools {
			if len(rule.Models) != 1 || (rule.Models[0] != "alpha" && rule.Models[0] != "beta") {
				t.Fatalf("saving tools did not expand the wildcard to current models: %+v", rule.Models)
			}
		}
	}
}

func TestServerToolsManagementClearOverridesYAMLAfterReload(t *testing.T) {
	dir := t.TempDir()
	raw := serverToolsManagementRegistration(dir, "  - models: [alpha]\n    tools:\n      - type: vercel:exa_search\n")
	// Old settings without the new field still inherit the YAML declaration.
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"debug":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	s := registerServerToolsManagement(t, raw)
	if got := preparedServerTools(t, s, "alpha"); bytes.Equal(got, []byte("null")) {
		t.Fatal("legacy settings unexpectedly removed the YAML tool")
	}
	if status, payload := modelServerToolsCall(t, s, "PUT", `{"model":"alpha","tools":[]}`); status != 200 {
		t.Fatalf("clear alpha: status=%d body=%v", status, payload)
	}
	saved, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(saved, &fields); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fields["server_tools"], []byte("[]")) {
		t.Fatalf("cleared tools were not explicitly persisted: %s", fields["server_tools"])
	}
	for _, current := range []*Service{s, registerServerToolsManagement(t, raw)} {
		if got := preparedServerTools(t, current, "alpha"); !bytes.Equal(got, []byte("null")) {
			t.Fatalf("cleared YAML tools returned: %s", got)
		}
		if !current.config().Debug {
			t.Fatal("clearing tools changed an unrelated setting")
		}
	}
}

func TestServerToolsManagementSerializesWithConfigSave(t *testing.T) {
	raw := serverToolsManagementRegistration(t.TempDir(), "  - models: [alpha]\n    tools:\n      - type: vercel:exa_search\n")
	s := registerServerToolsManagement(t, raw)
	type result struct {
		out any
		err error
	}
	call := func(path, body string, done chan<- result) {
		out, err := s.management(jsonBytes(ManagementRequest{Method: "PUT", Path: apiBase + path, Body: []byte(body)}))
		done <- result{out, err}
	}
	configDone, toolsDone := make(chan result, 1), make(chan result, 1)
	// Keep the first save in its credential-registration phase. The second save
	// must wait for that transaction rather than publishing another snapshot.
	s.credentialMu.Lock()
	var release sync.Once
	unlock := func() { release.Do(s.credentialMu.Unlock) }
	defer unlock()
	go call("/config", `{"debug":true}`, configDone)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(3 * time.Second)
	defer timeout.Stop()
	for !s.config().Debug {
		select {
		case <-ticker.C:
		case <-timeout.C:
			t.Fatal("config save never reached credential registration")
		}
	}
	go call("/models/server-tools", `{"model":"beta","tools":["vercel:tako_search"]}`, toolsDone)
	select {
	case <-toolsDone:
		t.Fatal("tool save completed during another configuration transaction")
	case <-time.After(100 * time.Millisecond):
	}
	publishedEarly := len(s.config().effectiveServerTools()["beta"]) != 0
	unlock()
	for _, done := range []<-chan result{configDone, toolsDone} {
		select {
		case r := <-done:
			response, ok := r.out.(ManagementResponse)
			if r.err != nil || !ok || response.StatusCode != 200 {
				t.Fatalf("concurrent save failed: response=%+v error=%v", r.out, r.err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("concurrent save did not finish")
		}
	}
	if publishedEarly {
		t.Fatal("tool save published a new configuration before the preceding save completed")
	}
	for _, current := range []*Service{s, registerServerToolsManagement(t, raw)} {
		cfg := current.config()
		if !cfg.Debug || len(cfg.effectiveServerTools()["alpha"]) != 1 || len(cfg.effectiveServerTools()["beta"]) != 1 {
			t.Fatalf("concurrent save lost a configuration update: %s", jsonBytes(cfg))
		}
	}
}
