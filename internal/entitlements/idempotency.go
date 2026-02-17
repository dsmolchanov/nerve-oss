package entitlements

type IdempotencyReplayError struct {
	Response any
}

func (e *IdempotencyReplayError) Error() string {
	return "idempotent replay"
}

type IdempotencyInProgressError struct {
	RetryAfterSeconds int
}

func (e *IdempotencyInProgressError) Error() string {
	return "idempotency in progress"
}
