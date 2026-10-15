package bridge

import (
	"bytes"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"
)

type streamIdleBackpressureResult struct {
	cp      *completion
	err     error
	elapsed time.Duration
}

func startStreamIdleBackpressureConsumer(s *Service, entry *LogEntry, timeout time.Duration, emit func([]byte) error) <-chan streamIdleBackpressureResult {
	s.cfg.StreamIdleCutoffEnabled = true
	s.cfg.StreamIdleTimeoutSeconds = 20
	started := time.Now()
	result := make(chan streamIdleBackpressureResult, 1)
	go func() {
		attempt := Attempt{}
		cp, err := s.consumeSSE(upstreamStream{
			StreamID: "backpressure-upstream", StatusCode: 200,
			Headers:  http.Header{"Content-Type": []string{"text/event-stream"}},
			deadline: started.Add(timeout),
		}, "fixture-model", entry, &attempt, started, emit)
		result <- streamIdleBackpressureResult{cp: cp, err: err, elapsed: time.Since(started)}
	}()
	synctest.Wait()
	return result
}

func requireStreamIdleBackpressurePending(t *testing.T, h *streamIdleFixture, result <-chan streamIdleBackpressureResult) {
	t.Helper()
	synctest.Wait()
	select {
	case <-h.closed:
		t.Fatal("upstream closed before its idle/deadline budget expired")
	default:
	}
	select {
	case got := <-result:
		t.Fatalf("consumer returned early at %s: %v", got.elapsed, got.err)
	default:
	}
}

func requireStreamIdleBackpressureResult(t *testing.T, result <-chan streamIdleBackpressureResult, kind string, elapsed time.Duration) streamIdleBackpressureResult {
	t.Helper()
	synctest.Wait()
	select {
	case got := <-result:
		var fault *upstreamError
		if kind == "" && got.err != nil || kind != "" && (!errors.As(got.err, &fault) || fault.Kind != kind) {
			t.Fatalf("consumer error = %v, want kind %q", got.err, kind)
		}
		if got.elapsed != elapsed {
			t.Fatalf("consumer returned at %s, want %s", got.elapsed, elapsed)
		}
		return got
	default:
		t.Fatalf("consumer is still pending at the expected %s boundary", elapsed)
		return streamIdleBackpressureResult{}
	}
}

func TestStreamIdleBackpressureBufferedCompletion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := NewService()
		s.cfg.Debug = true
		frames := []map[string]any{
			{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "first"}}}},
			{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "already-buffered-second"}}}},
			{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}},
		}
		var body []byte
		for _, frame := range frames {
			body = append(body, sseFrame(frame)...)
		}
		body = append(body, []byte("data: [DONE]\n\n")...)
		host := newFakeHost()
		host.streams["backpressure-upstream"] = []readChunk{{Payload: body, Done: true}}
		started := time.Now()
		var closedAt time.Duration
		emits := 0
		s.SetHost(func(method string, payload, out any) error {
			if method == "host.stream.emit" {
				emits++
				if emits == 1 {
					time.Sleep(21 * time.Second)
				}
			}
			if method == "host.http.stream_close" {
				closedAt = time.Since(started)
			}
			return host.call(method, payload, out)
		})
		entry := LogEntry{}
		result := startStreamIdleBackpressureConsumer(s, &entry, time.Minute, func(payload []byte) error {
			return s.call("host.stream.emit", map[string]any{"stream_id": "fixture-client", "payload": payload}, nil)
		})
		time.Sleep(21 * time.Second)
		got := requireStreamIdleBackpressureResult(t, result, "", 21*time.Second)
		if got.cp == nil || !got.cp.done || entry.StreamEnd != "done" || entry.EffectiveFrameCount != 2 {
			t.Fatalf("buffered completion was truncated: completion=%#v entry=%#v", got.cp, entry)
		}
		if emits != 3 || len(host.emitted) != 3 || host.reads["backpressure-upstream"] != 1 {
			t.Fatalf("buffered output: emits=%d acknowledged=%d reads=%d", emits, len(host.emitted), host.reads["backpressure-upstream"])
		}
		for i, frame := range frames {
			frame["model"] = "fixture-model"
			if !bytes.Equal(host.emitted[i], jsonBytes(frame)) {
				t.Fatalf("emitted frame %d = %s, want %s", i, host.emitted[i], jsonBytes(frame))
			}
		}
		if len(host.upstreamClosed) != 1 || closedAt != 21*time.Second {
			t.Fatalf("upstream closes=%d at=%s, want one close after emit resumes", len(host.upstreamClosed), closedAt)
		}
		d := entry.StreamDiagnostics
		if d == nil || d.DownstreamEmitMaxMS != 21000 || d.FirstEffectiveKind != "text" || d.LastEffectiveKind != "text" || d.IdleWaitMS < 0 || d.IdleWaitMS > 1 {
			t.Fatalf("backpressure diagnostics = %#v, want 21000ms emit, text output and near-zero idle", d)
		}
	})
}

func TestStreamIdleBackpressureIdleAfterEmit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := NewService()
		h := newStreamIdleFixture(s)
		defer close(h.input)
		entry := LogEntry{}
		result := startStreamIdleBackpressureConsumer(s, &entry, time.Minute, func(payload []byte) error {
			time.Sleep(21 * time.Second)
			return s.call("host.stream.emit", map[string]any{"stream_id": "fixture-client", "payload": payload}, nil)
		})
		h.feed(idleDeltaFrame(map[string]any{"content": "first"}))
		time.Sleep(21 * time.Second)
		requireStreamIdleBackpressurePending(t, h, result)
		if len(h.host.emitted) != 1 {
			t.Fatal("first emit did not resume after 21 seconds")
		}
		time.Sleep(19 * time.Second)
		requireStreamIdleBackpressurePending(t, h, result)
		time.Sleep(time.Second)
		requireStreamIdleBackpressureResult(t, result, upstreamStreamIdleTimeoutKind, 41*time.Second)
		if entry.StreamEnd != "idle_timeout" || entry.EffectiveFrameCount != 1 || len(h.host.upstreamClosed) != 1 {
			t.Fatalf("idle result: end=%q effective=%d closes=%d", entry.StreamEnd, entry.EffectiveFrameCount, len(h.host.upstreamClosed))
		}
	})
}

func TestStreamIdleBackpressureKeepsTotalDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := NewService()
		h := newStreamIdleFixture(s)
		defer close(h.input)
		entry := LogEntry{}
		emits := 0
		result := startStreamIdleBackpressureConsumer(s, &entry, 10*time.Second, func(payload []byte) error {
			emits++
			if emits == 1 {
				time.Sleep(21 * time.Second)
			}
			return s.call("host.stream.emit", map[string]any{"stream_id": "fixture-client", "payload": payload}, nil)
		})
		body := idleDeltaFrame(map[string]any{"content": "first"})
		body = append(body, sseFrame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})...)
		body = append(body, []byte("data: [DONE]\n\n")...)
		h.feed(body)
		time.Sleep(9 * time.Second)
		requireStreamIdleBackpressurePending(t, h, result)
		time.Sleep(time.Second)
		synctest.Wait()
		select {
		case <-h.closed:
		default:
			t.Fatal("blocked emit paused the total request deadline")
		}
		select {
		case got := <-result:
			t.Fatalf("consumer returned before emit resumed: %v", got.err)
		default:
		}
		time.Sleep(11 * time.Second)
		got := requireStreamIdleBackpressureResult(t, result, requestDeadlineKind, 21*time.Second)
		if got.cp == nil || got.cp.done || emits != 1 || entry.EffectiveFrameCount != 1 || len(h.host.upstreamClosed) != 1 {
			t.Fatalf("deadline bypassed by buffered finish: completion=%#v emits=%d effective=%d closes=%d", got.cp, emits, entry.EffectiveFrameCount, len(h.host.upstreamClosed))
		}
	})
}

func TestStreamIdleBackpressurePreservesSpentIdleBudget(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame []byte
	}{
		{"heartbeat", append([]byte(": heartbeat\n\n"), sseFrame(map[string]any{"type": "heartbeat"})...)},
		{"metadata", sseFrame(map[string]any{"provider_metadata": map[string]any{"provider": "fixture"}})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := NewService()
				h := newStreamIdleFixture(s)
				defer close(h.input)
				entry := LogEntry{}
				emits := 0
				result := startStreamIdleBackpressureConsumer(s, &entry, time.Minute, func(payload []byte) error {
					emits++
					if emits == 2 {
						time.Sleep(21 * time.Second)
					}
					return s.call("host.stream.emit", map[string]any{"stream_id": "fixture-client", "payload": payload}, nil)
				})
				h.feed(idleDeltaFrame(map[string]any{"content": "first"}))
				time.Sleep(7 * time.Second)
				h.feed(tc.frame)
				time.Sleep(21 * time.Second)
				requireStreamIdleBackpressurePending(t, h, result)
				if len(h.host.emitted) != 2 {
					t.Fatal("non-effective emit did not resume after 21 seconds")
				}
				// Seven idle seconds were spent before the pause: only 13 remain.
				time.Sleep(12 * time.Second)
				requireStreamIdleBackpressurePending(t, h, result)
				time.Sleep(time.Second)
				requireStreamIdleBackpressureResult(t, result, upstreamStreamIdleTimeoutKind, 41*time.Second)
				if entry.StreamEnd != "idle_timeout" || entry.EffectiveFrameCount != 1 || emits != 2 || len(h.host.upstreamClosed) != 1 {
					t.Fatalf("non-effective frame reset idle: end=%q effective=%d emits=%d closes=%d", entry.StreamEnd, entry.EffectiveFrameCount, emits, len(h.host.upstreamClosed))
				}
			})
		})
	}
}
