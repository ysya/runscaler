package metrics

import "time"

// RuntimeStatus is a cheap, in-memory snapshot of runner provisioning. Failure
// reasons are fixed categories, never provider errors or credential contents.
type RuntimeStatus struct {
	UnexpectedExits          int64           `json:"unexpected_exits"`
	StartAttempts            int64           `json:"start_attempts"`
	StartFailures            int64           `json:"start_failures"`
	ConsecutiveStartFailures int64           `json:"consecutive_start_failures"`
	LastStartAttempt         time.Time       `json:"last_start_attempt,omitzero"`
	LastSuccessfulStart      time.Time       `json:"last_successful_start,omitzero"`
	FailureReason            string          `json:"failure_reason,omitempty"`
	WaitingForCapacity       bool            `json:"waiting_for_capacity"`
	Draining                 bool            `json:"draining"`
	Degraded                 bool            `json:"degraded"`
	Readiness                string          `json:"readiness,omitempty"`
	LastFailure              *FailureSummary `json:"last_failure,omitempty"`
	RetryState               string          `json:"retry_state,omitempty"`
	NextRetryAt              time.Time       `json:"next_retry_at,omitzero"`
}

type FailureSummary struct {
	Reason     string     `json:"reason"`
	Timestamp  time.Time  `json:"timestamp"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	OOMKilled  *bool      `json:"oom_killed,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}
