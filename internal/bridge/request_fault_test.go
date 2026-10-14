package bridge

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLocalRequestFaultHelpers(t *testing.T) {
	for _, test := range []struct {
		name   string
		make   func(string) error
		status int
		code   string
	}{
		{name: "cancellation", make: requestCanceled, status: 499, code: "request_canceled"},
		{name: "deadline", make: requestTimedOut, status: 504, code: "request_deadline_exceeded"},
	} {
		t.Run(test.name, func(t *testing.T) {
			const message = "local request terminated"
			var upstream *upstreamError
			err := test.make(message)
			if !errors.As(err, &upstream) || upstream.StatusCode() != test.status || upstream.Code() != test.code || upstream.Scope != "request" || !upstream.IsRequestScoped() || upstream.Error() != message {
				t.Fatalf("local request fault lost status, code, scope or message: %#v", err)
			}
			if !upstream.RetryAt.IsZero() || upstream.QuotaType != "" || asTeamRateLimit(err) != nil {
				t.Fatalf("local request fault acquired a quota or rate-limit penalty: %#v", upstream)
			}
		})
	}
}

func TestEgressContextErrorPreservesLocalRequestFault(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	deadline, cancelDeadline := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelDeadline()
	deadlineCause, cancelCause := context.WithCancelCause(context.Background())
	cancelCause(context.DeadlineExceeded)
	for _, test := range []struct {
		name    string
		ctx     context.Context
		status  int
		code    string
		message string
	}{
		{name: "canceled", ctx: canceled, status: 499, code: requestCanceledKind, message: "egress request canceled"},
		{name: "deadline", ctx: deadline, status: 504, code: requestDeadlineKind, message: "egress request deadline exceeded"},
		{name: "deadline_cause", ctx: deadlineCause, status: 504, code: requestDeadlineKind, message: "egress request deadline exceeded"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var upstream *upstreamError
			err := egressContextError(test.ctx)
			if !errors.As(err, &upstream) || upstream.StatusCode() != test.status || upstream.Code() != test.code || upstream.Scope != "request" || !upstream.IsRequestScoped() || upstream.Error() != test.message {
				t.Fatalf("egress context termination lost typed request fault: %#v", err)
			}
		})
	}
}
