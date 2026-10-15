package bridge

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"testing/synctest"
	"time"
)

func TestDebugConfigDefaultsAndPersistence(t *testing.T) {
	for _, legacySettings := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy-yaml", true: "legacy-settings"}[legacySettings], func(t *testing.T) {
			dir := t.TempDir()
			if legacySettings {
				if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"timeout_seconds":120}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			s := observabilityService(t, dir)
			response := observabilityManagement(t, s, "GET", "/config", nil, nil, 200)
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(response.Body, &fields); err != nil {
				t.Fatal(err)
			}
			if s.config().Debug || string(fields["debug"]) != "false" {
				t.Fatalf("legacy configuration must expose debug=false: %s", response.Body)
			}
			want := s.config()
			for _, enabled := range []bool{true, false} {
				response = observabilityManagement(t, s, "PUT", "/config", nil, jsonBytes(map[string]bool{"debug": enabled}), 200)
				want.Debug = enabled
				var returned Config
				if err := json.Unmarshal(response.Body, &returned); err != nil || !reflect.DeepEqual(returned, want) {
					t.Fatalf("debug-only PUT changed other settings: %s (%v)", response.Body, err)
				}
				// An unrelated partial save must keep both explicit true and false.
				observabilityManagement(t, s, "PUT", "/config", nil, []byte(`{"log_retention":250}`), 200)
				want.LogRetention = 250
				assertStreamIdleConfigPersisted(t, s, want)
			}
		})
	}
}

func TestDebugStreamRecording(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			name := map[bool]string{false: "off", true: "on"}[enabled] + map[bool]string{false: "/aggregate", true: "/stream"}[stream]
			t.Run(name, func(t *testing.T) {
				s := observabilityService(t, "")
				observabilityManagement(t, s, "PUT", "/config", nil, jsonBytes(map[string]bool{"debug": enabled}), 200)
				h := newFakeHost(ssePlan(simpleSSE()))
				s.SetHost(h.call)
				method, streamID := "executor.execute", ""
				if stream {
					method, streamID = "executor.execute_stream", "fixture-client"
				}
				if _, err := s.Handle(method, executorRequest(streamID)); err != nil {
					t.Fatal(err)
				}
				s.active.Wait()
				if len(s.logs) != 1 {
					t.Fatalf("request log count = %d", len(s.logs))
				}
				entry := s.logs[0]
				if entry.Status != 200 || entry.StreamEnd != "done" || entry.EffectiveFrameCount != 1 || entry.PromptTokens != 8 || entry.CompletionTokens != 2 || !entry.UsageReported {
					t.Fatalf("debug=%t lost normal request metadata: %#v", enabled, entry)
				}
				if enabled {
					d := entry.StreamDiagnostics
					if d == nil || d.UpstreamReadCount != 1 || d.LastFrameKind != "done" || d.FirstEffectiveKind != "text" || d.LastUpstreamByteAt == nil || d.LastFrameAt == nil || (stream && d.DownstreamEmitCount != 2) {
						t.Fatalf("debug stream diagnostics = %#v", d)
					}
				} else if entry.StreamDiagnostics != nil {
					t.Fatal("debug-off request allocated stream diagnostics")
				}
				raw, err := os.ReadFile(filepath.Join(s.config().DataDir, "requests.json"))
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(raw, []byte(`"stream_diagnostics"`)) != enabled {
					t.Fatalf("persisted diagnostics presence does not match debug=%t", enabled)
				}
			})
		}
	}
}

func TestDebugLogsHideAndRestoreHistory(t *testing.T) {
	s := observabilityService(t, "")
	response := observabilityManagement(t, s, "GET", "/logs", nil, nil, 200)
	if !bytes.Contains(response.Body, []byte(`"debug":false`)) {
		t.Fatalf("empty logs omitted current debug=false: %s", response.Body)
	}
	observabilityManagement(t, s, "PUT", "/config", nil, []byte(`{"debug":true}`), 200)
	history := LogEntry{ID: "history", Status: 200, PromptTokens: 8, StreamDiagnostics: &StreamDiagnostics{UpstreamReadCount: 3, LastFrameKind: "done"}}
	s.appendLog(history)
	// This completed stream is still waiting to append its request log.
	pending := LogEntry{ID: "in-flight", Status: 502, ErrorKind: upstreamStreamIdleTimeoutKind, StreamEnd: "idle_timeout", EffectiveFrameCount: 1, StreamDiagnostics: &StreamDiagnostics{UpstreamReadCount: 1}}
	observabilityManagement(t, s, "PUT", "/config", nil, []byte(`{"debug":false}`), 200)
	s.appendLog(pending)
	path := filepath.Join(s.config().DataDir, "requests.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Reload while disabled, so persisted history must survive as well as memory.
	s = observabilityService(t, s.config().DataDir)
	for _, enabled := range []bool{false, true} {
		if enabled {
			observabilityManagement(t, s, "PUT", "/config", nil, []byte(`{"debug":true}`), 200)
		}
		response = observabilityManagement(t, s, "GET", "/logs", nil, nil, 200)
		var view struct {
			Debug *bool      `json:"debug"`
			Items []LogEntry `json:"items"`
		}
		if err := json.Unmarshal(response.Body, &view); err != nil {
			t.Fatal(err)
		}
		if view.Debug == nil || *view.Debug != enabled || len(view.Items) != 2 {
			t.Fatalf("unexpected logs response: %s", response.Body)
		}
		for _, entry := range view.Items {
			switch entry.ID {
			case "history":
				if (entry.StreamDiagnostics != nil) != enabled || entry.PromptTokens != 8 {
					t.Fatalf("history visibility did not follow debug=%t: %#v", enabled, entry)
				}
			case "in-flight":
				if entry.StreamDiagnostics != nil || entry.Status != 502 || entry.ErrorKind != upstreamStreamIdleTimeoutKind || entry.StreamEnd != "idle_timeout" || entry.EffectiveFrameCount != 1 {
					t.Fatalf("late append retained diagnostics or lost core error fields: %#v", entry)
				}
			default:
				t.Fatalf("unexpected request ID %q", entry.ID)
			}
		}
		if !enabled && bytes.Contains(response.Body, []byte(`"stream_diagnostics"`)) {
			t.Fatal("debug-off API exposed historical stream_diagnostics")
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("debug visibility changed persisted request history: %v", err)
		}
	}
}

func TestDebugDisabledDuringStreamStopsCollection(t *testing.T) {
	for _, reenable := range []bool{false, true} {
		t.Run(map[bool]string{false: "stays-off", true: "reenabled-before-finish"}[reenable], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := observabilityService(t, "")
				observabilityManagement(t, s, "PUT", "/config", nil, []byte(`{"debug":true}`), 200)
				h := newStreamIdleFixture(s)
				defer close(h.input)
				entry, attempt := LogEntry{}, Attempt{}
				result := make(chan error, 1)
				go func() {
					_, err := s.consumeSSE(upstreamStream{
						StreamID: "upstream-1", StatusCode: 200,
						Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
					}, "fixture", &entry, &attempt, time.Now(), func([]byte) error { return nil })
					result <- err
				}()
				h.feed(idleDeltaFrame(map[string]any{"content": "first"}))
				d := entry.StreamDiagnostics
				if d == nil || d.UpstreamReadCount != 1 || d.DownstreamEmitCount != 1 {
					t.Fatalf("debug-on stream did not start collection: %#v", d)
				}
				before := jsonBytes(d)
				observabilityManagement(t, s, "PUT", "/config", nil, []byte(`{"debug":false}`), 200)
				h.feed(idleDeltaFrame(map[string]any{"reasoning_content": "second"}))
				if !bytes.Equal(before, jsonBytes(d)) {
					t.Fatal("in-flight stream kept collecting after debug was disabled")
				}
				if reenable {
					observabilityManagement(t, s, "PUT", "/config", nil, []byte(`{"debug":true}`), 200)
				}
				h.feed(simpleSSE())
				if err := <-result; err != nil {
					t.Fatal(err)
				}
				if entry.StreamDiagnostics != nil || !bytes.Equal(before, jsonBytes(d)) || entry.StreamEnd != "done" || entry.EffectiveFrameCount != 3 || entry.PromptTokens != 8 || entry.CompletionTokens != 2 {
					t.Fatalf("disabled stream lost core metadata or retained diagnostics: %#v", entry)
				}
				entry.Status = 200
				s.appendLog(entry)
				if s.logs[0].StreamDiagnostics != nil {
					t.Fatal("in-flight diagnostics were persisted after debug was disabled")
				}
			})
		})
	}
}
