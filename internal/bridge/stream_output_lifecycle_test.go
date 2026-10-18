package bridge

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestStreamOutputDeadlineAndShutdownInterruptBlockedEmit(t *testing.T) {
	for _, reason := range []string{"deadline", "shutdown"} {
		for _, acknowledged := range []bool{false, true} {
			for _, supported := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/acknowledged=%t/details=%t", reason, acknowledged, supported), func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						s := registeredService(t, "stream-aggregate")
						s.cfg.TimeoutSeconds = 10
						body := append(idleDeltaFrame(map[string]any{"content": "first"}), idleDeltaFrame(map[string]any{"content": "second"})...)
						h := newFakeHost(ssePlan(body))
						blocked, outputClosed := make(chan struct{}), make(chan struct{})
						var blockOnce, closeOnce sync.Once
						emits, closes := 0, 0
						var details streamErrorDetails
						s.SetHost(func(method string, payload, out any) error {
							switch method {
							case "host.stream.emit":
								emits++
								if acknowledged && emits == 1 {
									return h.call(method, payload, out)
								}
								blockOnce.Do(func() { close(blocked) })
								// Model stock CPA's full output queue: only closing the
								// downstream, rather than the upstream, releases emit.
								<-outputClosed
								return errors.New("stream is not open")
							case "host.stream.close":
								closes++
								details, _ = payload.(map[string]any)["error_details"].(streamErrorDetails)
								*out.(*streamCloseResponse) = streamCloseResponse{ErrorDetailsSupported: supported}
								closeOnce.Do(func() { close(outputClosed) })
								return nil
							case "host.http.stream_close":
								if reason == "deadline" {
									select {
									case <-outputClosed:
									default:
										t.Error("deadline waited for upstream cleanup before unblocking the downstream")
									}
								}
							}
							return h.call(method, payload, out)
						})
						handoff := make(chan error, 1)
						go func() {
							_, err := s.Handle("executor.execute_stream", executorRequest("blocked-client"))
							handoff <- err
						}()
						<-blocked
						if err := <-handoff; err != nil {
							t.Fatal(err)
						}
						if reason == "deadline" {
							time.Sleep(10 * time.Second)
						} else if _, err := s.Handle("plugin.shutdown", nil); err != nil {
							t.Fatal(err)
						}
						s.active.Wait()
						synctest.Wait()
						wantStatus, wantCode := 504, requestDeadlineKind
						if reason == "shutdown" {
							wantStatus, wantCode = 503, bridgeUnavailableKind
						}
						wantPhase := "before_output"
						if acknowledged {
							wantPhase = "after_output"
						}
						if closes != 1 || details.HTTPStatus != wantStatus || details.Code != wantCode || details.Phase != wantPhase || !details.RequestScoped {
							t.Fatalf("close calls=%d details=%#v", closes, details)
						}
						if len(s.logs) != 1 {
							t.Fatalf("request logs=%d", len(s.logs))
						}
						entry := s.logs[0]
						wantDisposition := "legacy_host_unknown"
						if supported {
							wantDisposition = "exempt"
						}
						if entry.Status != wantStatus || entry.ErrorKind != wantCode || entry.ErrorPhase != wantPhase || entry.DownstreamStarted == nil || *entry.DownstreamStarted != acknowledged || entry.StreamErrorDetailsSupported == nil || *entry.StreamErrorDetailsSupported != supported || entry.CooldownDisposition != wantDisposition {
							t.Fatalf("termination receipt lost: %#v", entry)
						}
						if reason == "deadline" {
							if _, err := s.Handle("plugin.shutdown", nil); err != nil {
								t.Fatal(err)
							}
						}
					})
				})
			}
		}
	}
}
