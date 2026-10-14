package bridge

const (
	requestCanceledKind = "request_canceled"
	requestDeadlineKind = "request_deadline_exceeded"
)

// These helpers describe local request termination, never an upstream HTTP response.
func requestCanceled(message string) error {
	return &upstreamError{
		APIError: &APIError{Status: 499, Kind: requestCanceledKind, Message: message},
		Scope:    "request",
	}
}

func requestTimedOut(message string) error {
	return &upstreamError{
		APIError: &APIError{Status: 504, Kind: requestDeadlineKind, Message: message},
		Scope:    "request",
	}
}
