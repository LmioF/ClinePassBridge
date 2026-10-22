package bridge

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func networkLogMetadata(key string, counts any) map[string]any {
	return map[string]any{key: map[string]any{"gateway": map[string]any{"gatewayToolCalls": counts}}}
}

func networkLogFrame(count int64) map[string]any {
	frame := networkLogMetadata("provider_metadata", map[string]any{"exa_search": count, "perplexity_search": 0})
	frame["choices"] = []any{map[string]any{
		"index": 0,
		"delta": networkLogMetadata("providerMetadata", map[string]any{"exa_search": count}),
	}}
	return frame
}

func TestNetworkLogExecutorOutboundAndCounts(t *testing.T) {
	for _, transport := range []struct{ mode, method string }{
		{"native", "executor.execute"},
		{"stream-aggregate", "executor.execute"},
		{"native", "executor.execute_stream"},
	} {
		t.Run(transport.mode+"/"+transport.method, func(t *testing.T) {
			s := registeredService(t, transport.mode)
			s.cfg.ServerTools = []ServerToolRule{{Models: []string{"deepseek-flash"}, Tools: []map[string]any{
				{"type": "vercel:exa_search"}, {"type": "vercel:tako_search"}, {"type": "vercel:browserbase_fetch"},
			}}}
			message := networkLogMetadata("providerMetadata", map[string]any{"exa_search": 3, "perplexity_search": 0})
			message["role"], message["content"] = "assistant", "hello"
			completion := networkLogMetadata("provider_metadata", map[string]any{"exa_search": 1})
			completion["choices"] = []any{map[string]any{"index": 0, "message": message, "finish_reason": "stop"}}
			plan := jsonPlan(map[string]any{"success": true, "data": completion,
				"providerMetadata": object(networkLogMetadata("providerMetadata", map[string]any{"exa_search": 3})["providerMetadata"]),
			})
			if transport.mode != "native" || transport.method == "executor.execute_stream" {
				plan = ssePlan(
					sseFrame(networkLogFrame(1)), sseFrame(networkLogFrame(3)),
					sseFrame(networkLogFrame(2)), sseFrame(networkLogFrame(3)), simpleSSE(),
				)
			}
			h, bodies := reasoningMappingHost(t, s, plan)
			var req ExecutorRequest
			if err := json.Unmarshal(executorRequest("network-log-client"), &req); err != nil {
				t.Fatal(err)
			}
			req.Payload = []byte(`{"messages":[{"role":"user","content":"prompt-secret"}],"tools":[{"type":"vercel:exa_search","options":{"marker":"config-secret"}},{"type":"vercel:perplexity_search"},{"type":"function","function":{"name":"tako_search","description":"schema-secret","parameters":{"type":"object"}}},{"type":"vercel:future_search"}]}`)
			if _, err := s.Handle(transport.method, jsonBytes(req)); err != nil {
				t.Fatal(err)
			}
			if transport.method == "executor.execute_stream" {
				select {
				case <-h.clientClosed:
				case <-time.After(3 * time.Second):
					t.Fatal("stream did not close")
				}
				s.active.Wait()
			}
			body := <-bodies
			wantTools := []string{"vercel:exa_search", "vercel:perplexity_search", "vercel:browserbase_fetch"}
			if got := toolTypes(body); !reflect.DeepEqual(got, []string{"vercel:exa_search", "vercel:perplexity_search", "function", "vercel:future_search", "vercel:browserbase_fetch"}) {
				t.Fatalf("unexpected actual outbound tools: %v", got)
			}
			entry := s.logs[0]
			wantCalls := map[string]int64{"exa_search": 3, "perplexity_search": 0}
			if !reflect.DeepEqual(entry.ServerTools, wantTools) || !entry.ServerToolCallsReported || !reflect.DeepEqual(entry.ServerToolCalls, wantCalls) {
				t.Fatalf("request network log = %#v", entry)
			}
			if len(entry.Attempts) != 1 || !reflect.DeepEqual(entry.Attempts[0].ServerTools, wantTools) || !entry.Attempts[0].ServerToolCallsReported || !reflect.DeepEqual(entry.Attempts[0].ServerToolCalls, wantCalls) {
				t.Fatalf("attempt network log = %#v", entry.Attempts)
			}
			for _, secret := range []string{"prompt-secret", "config-secret", "schema-secret"} {
				if bytes.Contains(jsonBytes(entry), []byte(secret)) {
					t.Fatalf("request log retained %q", secret)
				}
			}
			if s.config().Debug || entry.StreamDiagnostics != nil {
				t.Fatal("network logging must work with debug disabled")
			}
		})
	}
}

func TestNetworkLogTotalsAcrossFallbackAttempts(t *testing.T) {
	s := registeredService(t, "native-fallback")
	s.cfg.ServerTools = []ServerToolRule{{Models: []string{"deepseek-flash"}, Tools: []map[string]any{{"type": "vercel:exa_search"}}}}
	failure := networkLogMetadata("providerMetadata", map[string]any{"exa_search": 2})
	failure["error"] = "empty response content"
	h, _ := reasoningMappingHost(t, s, jsonErrorPlan(500, failure), ssePlan(
		sseFrame(networkLogFrame(1)), sseFrame(networkLogFrame(1)), sseFrame(networkLogFrame(3)), simpleSSE(),
	))
	if _, err := s.Handle("executor.execute", executorRequest("")); err != nil {
		t.Fatal(err)
	}
	entry := s.logs[0]
	if !reflect.DeepEqual(h.opened, []bool{false, true}) || len(entry.Attempts) != 2 {
		t.Fatalf("fallback attempts = %v / %#v", h.opened, entry.Attempts)
	}
	if !entry.ServerToolCallsReported || entry.ServerToolCalls["exa_search"] != 5 {
		t.Fatalf("total calls = %#v, want exa_search:5", entry.ServerToolCalls)
	}
	for i, count := range []int64{2, 3} {
		attempt := entry.Attempts[i]
		if !attempt.ServerToolCallsReported || attempt.ServerToolCalls["exa_search"] != count || !reflect.DeepEqual(attempt.ServerTools, []string{"vercel:exa_search"}) {
			t.Fatalf("attempt %d = %#v, want exa_search:%d", i, attempt, count)
		}
	}
}

func TestNetworkLogCounterReporting(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		reported  bool
		counts    map[string]int64
	}{
		{name: "missing"},
		{name: "empty", raw: `{}`, reported: true, counts: map[string]int64{}},
		{name: "zero", raw: `{"exa_search":0}`, reported: true, counts: map[string]int64{"exa_search": 0}},
		{name: "null", raw: `null`},
		{name: "array", raw: `[]`},
		{name: "negative", raw: `{"exa_search":-1}`},
		{name: "fraction", raw: `{"exa_search":1.5}`},
		{name: "string", raw: `{"exa_search":"2"}`},
		{name: "overflow", raw: `{"exa_search":9223372036854775808}`},
		{name: "partly-invalid", raw: `{"exa_search":2,"tako_search":false}`},
		{name: "unknown-only", raw: `{"future_search":2}`},
		{name: "invalid-name", raw: `{"exa_search ":2}`},
		{name: "known-and-unknown", raw: `{"exa_search":2,"future_search":8}`, reported: true, counts: map[string]int64{"exa_search": 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"provider_metadata":{"gateway":{}}}`
			if tc.raw != "" {
				raw = `{"provider_metadata":{"gateway":{"gatewayToolCalls":` + tc.raw + `}}}`
			}
			frame, err := decodeObject([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			var entry LogEntry
			var attempt Attempt
			observeMetadata(frame, &entry, &attempt)
			if entry.ServerToolCallsReported != tc.reported || attempt.ServerToolCallsReported != tc.reported || !reflect.DeepEqual(entry.ServerToolCalls, tc.counts) || !reflect.DeepEqual(attempt.ServerToolCalls, tc.counts) {
				t.Fatalf("reported/counts = %t/%v, attempt %t/%v", entry.ServerToolCallsReported, entry.ServerToolCalls, attempt.ServerToolCallsReported, attempt.ServerToolCalls)
			}
			// A later missing/invalid value cannot erase a valid report.
			observeMetadata(networkLogMetadata("providerMetadata", "invalid"), &entry, &attempt)
			if entry.ServerToolCallsReported != tc.reported || !reflect.DeepEqual(entry.ServerToolCalls, tc.counts) {
				t.Fatal("invalid metadata erased the previous report")
			}
		})
	}
}

func TestNetworkLogLocalFailuresDoNotReportToolsOrCalls(t *testing.T) {
	for _, method := range []string{"executor.execute", "executor.execute_stream"} {
		t.Run(method, func(t *testing.T) {
			s := registeredService(t, "native")
			s.cfg.ServerTools = []ServerToolRule{{Models: []string{"deepseek-flash"}, Tools: []map[string]any{{"type": "vercel:exa_search"}}}}
			reasoningMappingHost(t, s)
			if _, err := s.Handle(method, requestWithPayload(`{"messages":[]}`)); err == nil {
				t.Fatal("invalid request unexpectedly succeeded")
			}
			entry := s.logs[0]
			if entry.ServerTools != nil || entry.ServerToolCallsReported || entry.ServerToolCalls != nil {
				t.Fatalf("local failure reported upstream facts: %#v", entry)
			}
			for _, attempt := range entry.Attempts {
				if attempt.ServerTools != nil || attempt.ServerToolCallsReported || attempt.ServerToolCalls != nil {
					t.Fatalf("local failure attempt reported upstream facts: %#v", attempt)
				}
			}
		})
	}
}
