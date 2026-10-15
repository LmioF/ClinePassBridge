package bridge

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func idleDeltaFrame(delta map[string]any) []byte {
	return sseFrame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta}}})
}

func TestStreamIdleEffectiveDeltas(t *testing.T) {
	for _, test := range []struct {
		name  string
		delta map[string]any
		want  bool
	}{
		{"text", map[string]any{"content": "hello"}, true},
		{"whitespace increment", map[string]any{"content": " "}, true},
		{"reasoning content", map[string]any{"reasoning_content": "thinking"}, true},
		{"reasoning", map[string]any{"reasoning": "thinking"}, true},
		{"refusal", map[string]any{"refusal": "declined"}, true},
		{"legacy function name", map[string]any{"function_call": map[string]any{"name": "lookup"}}, true},
		{"legacy arguments", map[string]any{"function_call": map[string]any{"arguments": "{"}}, true},
		{"tool name", map[string]any{"tool_calls": []any{map[string]any{"function": map[string]any{"name": "lookup"}}}}, true},
		{"tool arguments", map[string]any{"tool_calls": []any{map[string]any{"function": map[string]any{"arguments": "}"}}}}, true},
		{"role", map[string]any{"role": "assistant"}, false},
		{"empty strings", map[string]any{"content": "", "reasoning": "", "refusal": nil}, false},
		{"empty legacy function", map[string]any{"function_call": map[string]any{"arguments": "", "name": ""}}, false},
		{"tool identity", map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call-1", "type": "function"}}}, false},
		{"empty tool function", map[string]any{"tool_calls": []any{map[string]any{"function": map[string]any{"name": "", "arguments": ""}}}}, false},
		{"metadata", map[string]any{"provider_metadata": map[string]any{"provider": "fixture"}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			frame := map[string]any{"choices": []any{map[string]any{"delta": test.delta}}}
			if contentStarted(frame) != test.want {
				t.Fatalf("effective output = %t, want %t", contentStarted(frame), test.want)
			}
		})
	}
}

func TestStreamIdleWatchdogResetsAndJoins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := NewService()
		var closes atomic.Int32
		s.SetHost(func(method string, _, _ any) error { closes.Add(1); return nil })
		w := s.newStreamReadWatchdog(upstreamStream{StreamID: "same-id", deadline: time.Now().Add(time.Minute)}, 10*time.Second)
		go w.run()
		time.Sleep(20 * time.Second)
		if w.failure() != nil || closes.Load() != 0 {
			t.Fatal("first output wait started the idle timer")
		}
		for range 3 {
			if err := w.effectiveOutput(); err != nil {
				t.Fatal(err)
			}
			time.Sleep(9 * time.Second)
			if w.failure() != nil || closes.Load() != 0 {
				t.Fatal("fresh output did not extend the idle deadline")
			}
		}
		time.Sleep(time.Second)
		synctest.Wait()
		var failure *upstreamError
		if !errors.As(w.failure(), &failure) || failure.Kind != upstreamStreamIdleTimeoutKind || closes.Load() != 1 {
			t.Fatalf("idle cutoff = %v, closes = %d", w.failure(), closes.Load())
		}
		w.stop()
		w.close()
		// A later stream can reuse the identifier after the old watchdog exits.
		time.Sleep(time.Minute)
		if closes.Load() != 1 {
			t.Fatalf("old watchdog closed a later stream: %d", closes.Load())
		}
	})
}

type streamIdleFixture struct {
	host      *fakeHost
	input     chan readChunk
	closed    chan struct{}
	closeOnce sync.Once
	details   streamErrorDetails
}

func newStreamIdleFixture(s *Service) *streamIdleFixture {
	h := &streamIdleFixture{host: newFakeHost(ssePlan()), input: make(chan readChunk), closed: make(chan struct{})}
	s.SetHost(func(method string, payload, out any) error {
		switch method {
		case "host.http.stream_read":
			select {
			case chunk, ok := <-h.input:
				if !ok {
					chunk.Done = true
				}
				*out.(*readChunk) = chunk
			case <-h.closed:
				*out.(*readChunk) = readChunk{Done: true}
			}
			return nil
		case "host.http.stream_close":
			h.closeOnce.Do(func() { close(h.closed) })
		case "host.stream.close":
			h.details, _ = payload.(map[string]any)["error_details"].(streamErrorDetails)
			*out.(*streamCloseResponse) = streamCloseResponse{ErrorDetailsSupported: true}
		}
		return h.host.call(method, payload, out)
	})
	return h
}

func (h *streamIdleFixture) feed(payload []byte) {
	h.input <- readChunk{Payload: payload}
	synctest.Wait()
}

func TestStreamIdleConsumerLifecycle(t *testing.T) {
	for _, scenario := range []string{"heartbeat", "continuous", "disabled", "done", "eof", "cancel", "deadline", "first_output_deadline", "native"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := NewService()
				s.cfg.StreamIdleCutoffEnabled = scenario != "disabled"
				h := newStreamIdleFixture(s)
				entry, attempt := LogEntry{}, Attempt{}
				deadline := time.Now().Add(time.Minute)
				if scenario == "deadline" || scenario == "first_output_deadline" {
					deadline = time.Now().Add(8 * time.Second)
				}
				up := upstreamStream{StreamID: "upstream-1", StatusCode: 200, Headers: http.Header{"Content-Type": []string{"text/event-stream"}}, deadline: deadline}
				result := make(chan error, 1)
				go func() {
					if scenario == "native" {
						_, err := s.readJSON(up)
						result <- err
						return
					}
					_, err := s.consumeSSE(up, "model", &entry, &attempt, time.Now(), nil)
					result <- err
				}()
				synctest.Wait()
				if scenario == "first_output_deadline" {
					time.Sleep(8 * time.Second)
				} else {
					h.feed(idleDeltaFrame(map[string]any{"content": "partial"}))
					switch scenario {
					case "heartbeat":
						for _, frame := range [][]byte{
							[]byte(": heartbeat\n\n"),
							[]byte("data: \n\n"),
							idleDeltaFrame(map[string]any{"role": "assistant"}),
							sseFrame(map[string]any{"usage": map[string]any{"completion_tokens": 1}}),
							idleDeltaFrame(map[string]any{"tool_calls": []any{map[string]any{"index": 0}}}),
						} {
							time.Sleep(time.Second)
							h.feed(frame)
						}
						time.Sleep(5 * time.Second)
					case "continuous":
						for _, delta := range []map[string]any{
							{"reasoning_content": "thinking"},
							{"refusal": "declined"},
							{"function_call": map[string]any{"name": "lookup"}},
							{"tool_calls": []any{map[string]any{"function": map[string]any{"arguments": "{"}}}},
						} {
							time.Sleep(9 * time.Second)
							h.feed(idleDeltaFrame(delta))
						}
						select {
						case err := <-result:
							t.Fatalf("effective deltas failed to reset idle timer: %v", err)
						default:
						}
						time.Sleep(10 * time.Second)
					case "deadline":
						time.Sleep(8 * time.Second)
					case "cancel":
						time.Sleep(9 * time.Second)
						h.input <- readChunk{ErrorDetails: &httpErrorDetails{Kind: "canceled", Code: requestCanceledKind, HTTPStatus: 499}, Done: true}
					case "disabled", "native":
						time.Sleep(20 * time.Second)
						synctest.Wait()
						select {
						case err := <-result:
							t.Fatalf("idle cutoff applied unexpectedly: %v", err)
						default:
						}
						if scenario == "disabled" {
							h.feed(sseFrame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}}))
						}
						close(h.input)
					case "done", "eof":
						time.Sleep(9 * time.Second)
						h.feed(sseFrame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}}))
						if scenario == "done" {
							h.feed([]byte("data: [DONE]\n\n"))
						} else {
							close(h.input)
						}
					}
				}
				err := <-result
				code := ""
				switch scenario {
				case "heartbeat", "continuous":
					code = upstreamStreamIdleTimeoutKind
				case "deadline", "first_output_deadline":
					code = requestDeadlineKind
				case "cancel":
					code = requestCanceledKind
				}
				var typed *upstreamError
				if code == "" && err != nil || code != "" && (!errors.As(err, &typed) || typed.Kind != code) {
					t.Fatalf("consumer error = %v, want %q", err, code)
				}
				if scenario == "heartbeat" && (entry.StreamEnd != "idle_timeout" || entry.EffectiveFrameCount != 1) {
					t.Fatalf("idle end/effective frames = %#v", entry)
				}
				time.Sleep(time.Minute)
				synctest.Wait()
				if len(h.host.upstreamClosed) != 1 {
					t.Fatalf("watchdog leaked after completion: %v", h.host.upstreamClosed)
				}
			})
		})
	}
}

func TestStreamIdleExecutorErrorBoundaries(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, compatibility := range []bool{false, true} {
			name := map[bool]string{false: "aggregate", true: "stream"}[stream] + map[bool]string{false: "/compat_off", true: "/compat_on"}[compatibility]
			t.Run(name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					s := registeredService(t, "stream-aggregate")
					s.cfg.ClientRetryCompatibility = compatibility
					s.cfg.StreamIdleCutoffEnabled = true
					s.cfg.StreamIdleTimeoutSeconds = 2
					h := newStreamIdleFixture(s)
					result := make(chan error, 1)
					go func() {
						method, streamID := "executor.execute", ""
						if stream {
							method, streamID = "executor.execute_stream", "client-1"
						}
						_, err := s.Handle(method, executorRequest(streamID))
						result <- err
					}()
					synctest.Wait()
					h.feed(idleDeltaFrame(map[string]any{"role": "assistant"}))
					time.Sleep(12 * time.Second)
					h.feed(idleDeltaFrame(map[string]any{"refusal": "partial"}))
					if stream {
						if err := <-result; err != nil {
							t.Fatalf("stream handoff failed: %v", err)
						}
					}
					time.Sleep(2 * time.Second)
					if stream {
						<-h.host.clientClosed
					} else {
						var upstream *upstreamError
						if err := <-result; !errors.As(err, &upstream) || upstream.Kind != upstreamStreamIdleTimeoutKind || !upstream.IsRequestScoped() || statusOf(err) != 502 {
							t.Fatalf("aggregate returned an incorrect fault: %v", err)
						}
					}
					s.active.Wait()
					synctest.Wait()
					if len(s.logs) != 1 || len(h.host.opened) != 1 || len(h.host.upstreamClosed) != 1 {
						t.Fatalf("unexpected retry/close/log: opens=%d closes=%d logs=%d", len(h.host.opened), len(h.host.upstreamClosed), len(s.logs))
					}
					entry := s.logs[0]
					if entry.Status != 502 || entry.ErrorKind != upstreamStreamIdleTimeoutKind || entry.RateLimitScope != "request" || entry.RetryAt != nil || entry.UpstreamHTTPStatus != 0 || entry.UpstreamSkipped || entry.StreamEnd != "idle_timeout" || entry.EffectiveFrameCount != 1 {
						t.Fatalf("idle log lost request-scoped diagnostics: %#v", entry)
					}
					if !strings.HasPrefix(entry.Error, "HTTP 502:") || !strings.Contains(entry.Error, upstreamStreamIdleTimeoutKind) {
						t.Fatalf("unclear idle message with compatibility=%t: %s", compatibility, entry.Error)
					}
					if stream && (h.details.Code != upstreamStreamIdleTimeoutKind || h.details.HTTPStatus != 502 || h.details.Scope != "request" || !h.details.RequestScoped || h.details.RetryAt != nil || h.details.Phase != "after_output" || entry.CooldownDisposition != "exempt" || h.host.clientError != entry.Error) {
						t.Fatalf("stream.close lost idle fault: details=%#v entry=%#v client=%q", h.details, entry, h.host.clientError)
					}
					if len(s.rateLimits.windows) != 0 || len(s.getEgressManager().groups) != 0 {
						t.Fatal("idle cutoff created a team/egress backoff")
					}
				})
			})
		}
	}
}

func TestStreamIdlePreservesParentCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		w := NewService().newStreamReadWatchdog(upstreamStream{deadline: time.Now().Add(10 * time.Second)}, 10*time.Second)
		w.closeStream = func() {}
		w.parentError = func() error {
			if ctx.Err() != nil {
				return requestCanceled("already canceled")
			}
			return nil
		}
		go w.run()
		if err := w.effectiveOutput(); err != nil {
			t.Fatal(err)
		}
		cancel()
		time.Sleep(10 * time.Second)
		synctest.Wait()
		w.stop()
		var upstream *upstreamError
		if !errors.As(w.failure(), &upstream) || upstream.Kind != requestCanceledKind {
			t.Fatalf("idle timer replaced an existing parent cancellation: %v", w.failure())
		}
	})
}

func TestStreamIdleDoesNotRetreatRecoveryProbe(t *testing.T) {
	now := time.Now()
	l := rateLimiter{clock: func() time.Time { return now }}
	c := Credential{ID: "fixture", APIKey: "fixture-key"}
	lease, err := l.acquire(c, "model")
	if err != nil {
		t.Fatal(err)
	}
	lease.finish(classifyUpstreamError(429, http.Header{"Retry-After": []string{"1"}}, map[string]any{"error": teamLimitMessage}, now))
	now = now.Add(2 * time.Second)
	probe, err := l.acquire(c, "model")
	if err != nil {
		t.Fatal(err)
	}
	deadline := l.windows[probe.key].retryAt
	finishRequestRateLease(probe, streamIdleTimeout(10*time.Second))
	if state := l.windows[probe.key]; state.probe != nil || !state.retryAt.Equal(deadline) {
		t.Fatalf("idle timeout extended recovery backoff: %#v", state)
	}
}
