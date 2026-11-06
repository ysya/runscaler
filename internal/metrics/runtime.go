package metrics

import "time"

// RuntimeStatus is a cheap, in-memory snapshot of runner provisioning. Failure
// reasons are fixed categories, never provider errors or credential contents.
type RuntimeStatus struct {
	StartAttempts            int64     `json:"start_attempts"`
	StartFailures            int64     `json:"start_failures"`
	ConsecutiveStartFailures int64     `json:"consecutive_start_failures"`
	LastStartAttempt         time.Time `json:"last_start_attempt,omitzero"`
	LastSuccessfulStart      time.Time `json:"last_successful_start,omitzero"`
	FailureReason            string    `json:"failure_reason,omitempty"`
	WaitingForCapacity       bool      `json:"waiting_for_capacity"`
	Draining                 bool      `json:"draining"`
	Degraded                 bool      `json:"degraded"`
}
