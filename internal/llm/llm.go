// Package llm hides the model provider behind a small interface so the agent
// runtime never depends on provider-specific types.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type ToolSpec struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

type ToolCall struct {
	ID        string
	Name      string
	Arguments string // raw JSON as produced by the model
}

type ToolOutput struct {
	CallID string
	Output string
}

// Request carries only the *new* input for this turn. Conversation history is
// held by the provider and referenced through PreviousID (as the original
// implementation did with previous_response_id).
type Request struct {
	Instructions string
	UserText     string
	ToolOutputs  []ToolOutput
	Tools        []ToolSpec
	PreviousID   string

	// OnDelta, if set, is called with each chunk of output text as it streams
	// in, in order, before Generate returns. It must be cheap and non-blocking.
	OnDelta func(string)
}

type Usage struct {
	InputTokens  int64
	OutputTokens int64
}

type Response struct {
	ID         string
	Text       string
	ToolCalls  []ToolCall
	Usage      Usage
	Incomplete string // non-empty if the model stopped early (e.g. max_output_tokens)
}

type Provider interface {
	Name() string
	Model() string
	Generate(ctx context.Context, req Request) (*Response, error)
}

type ErrKind int

const (
	ErrOther      ErrKind = iota
	ErrConfig             // missing or invalid configuration
	ErrAuth               // 401/403
	ErrRateLimit          // 429
	ErrTransient          // 5xx, timeouts, connection problems
	ErrBadRequest         // 4xx the caller must fix (including unsupported capability)
	ErrContext            // context window exceeded
	ErrCancelled
)

type Error struct {
	Kind       ErrKind
	Status     int
	RetryAfter time.Duration
	Err        error
}

func (e *Error) Error() string {
	switch e.Kind {
	case ErrAuth:
		return fmt.Sprintf("authentication failed (HTTP %d): check OPENAI_API_KEY and that the key may use this model", e.Status)
	case ErrRateLimit:
		return "rate limited by the provider: " + e.Err.Error()
	case ErrTransient:
		return "temporary provider/network failure: " + e.Err.Error()
	case ErrBadRequest:
		return fmt.Sprintf("provider rejected the request (HTTP %d): %v", e.Status, e.Err)
	case ErrContext:
		return "conversation exceeds the model's context window: " + e.Err.Error()
	}
	return e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

// Retryable is true only for failures that cannot have had side effects.
func (e *Error) Retryable() bool { return e.Kind == ErrRateLimit || e.Kind == ErrTransient }

// Policy configures bounded exponential backoff.
type RetryPolicy struct {
	MaxAttempts int
	Base        time.Duration
	Cap         time.Duration
	Sleep       func(ctx context.Context, d time.Duration) error // injectable for tests
	OnRetry     func(attempt int, wait time.Duration, err error)
}

func DefaultRetry() RetryPolicy {
	return RetryPolicy{MaxAttempts: 4, Base: time.Second, Cap: 30 * time.Second}
}

type retrying struct {
	Provider
	p RetryPolicy
}

// WithRetry wraps a provider with retries for transient failures only.
func WithRetry(p Provider, pol RetryPolicy) Provider {
	if pol.Sleep == nil {
		pol.Sleep = sleep
	}
	if pol.MaxAttempts < 1 {
		pol.MaxAttempts = 1
	}
	return &retrying{Provider: p, p: pol}
}

func (r *retrying) Generate(ctx context.Context, req Request) (*Response, error) {
	var lastErr error
	for attempt := 1; attempt <= r.p.MaxAttempts; attempt++ {
		resp, err := r.Provider.Generate(ctx, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		var le *Error
		if !errors.As(err, &le) || !le.Retryable() || attempt == r.p.MaxAttempts || ctx.Err() != nil {
			return nil, err
		}
		wait := r.p.Base << (attempt - 1)
		if le.RetryAfter > 0 {
			wait = le.RetryAfter
		}
		wait = min(wait, r.p.Cap)
		if r.p.OnRetry != nil {
			r.p.OnRetry(attempt, wait, err)
		}
		if err := r.p.Sleep(ctx, wait); err != nil {
			return nil, &Error{Kind: ErrCancelled, Err: err}
		}
	}
	return nil, lastErr
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
