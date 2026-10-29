package llm

import (
	"context"
	"errors"
	"testing"
	"time"
)

type flaky struct {
	errs  []error
	calls int
}

func (f *flaky) Name() string  { return "flaky" }
func (f *flaky) Model() string { return "m" }
func (f *flaky) Generate(context.Context, Request) (*Response, error) {
	f.calls++
	if f.calls <= len(f.errs) {
		return nil, f.errs[f.calls-1]
	}
	return &Response{Text: "ok"}, nil
}

func policy(waits *[]time.Duration) RetryPolicy {
	return RetryPolicy{MaxAttempts: 4, Base: time.Second, Cap: 5 * time.Second,
		Sleep: func(_ context.Context, d time.Duration) error { *waits = append(*waits, d); return nil }}
}

func TestRetriesTransientWithBoundedBackoff(t *testing.T) {
	var waits []time.Duration
	f := &flaky{errs: []error{
		&Error{Kind: ErrTransient, Err: errors.New("503")},
		&Error{Kind: ErrRateLimit, Err: errors.New("429"), RetryAfter: 3 * time.Second},
		&Error{Kind: ErrTransient, Err: errors.New("503")},
	}}
	resp, err := WithRetry(f, policy(&waits)).Generate(context.Background(), Request{})
	if err != nil || resp.Text != "ok" || f.calls != 4 {
		t.Fatalf("%v %v calls=%d", resp, err, f.calls)
	}
	want := []time.Duration{time.Second, 3 * time.Second, 4 * time.Second}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("waits %v want %v", waits, want)
		}
	}
}

func TestDoesNotRetryPermanentErrors(t *testing.T) {
	for _, k := range []ErrKind{ErrAuth, ErrBadRequest, ErrConfig, ErrContext} {
		var waits []time.Duration
		f := &flaky{errs: []error{&Error{Kind: k, Status: 400, Err: errors.New("no")}, nil}}
		_, err := WithRetry(f, policy(&waits)).Generate(context.Background(), Request{})
		if err == nil || f.calls != 1 || len(waits) != 0 {
			t.Fatalf("kind %d: err=%v calls=%d", k, err, f.calls)
		}
	}
}

func TestGivesUpAfterMaxAttempts(t *testing.T) {
	var waits []time.Duration
	f := &flaky{errs: []error{
		&Error{Kind: ErrTransient, Err: errors.New("a")}, &Error{Kind: ErrTransient, Err: errors.New("b")},
		&Error{Kind: ErrTransient, Err: errors.New("c")}, &Error{Kind: ErrTransient, Err: errors.New("d")}, &Error{Kind: ErrTransient, Err: errors.New("e")}}}
	_, err := WithRetry(f, policy(&waits)).Generate(context.Background(), Request{})
	if err == nil || f.calls != 4 {
		t.Fatalf("err=%v calls=%d", err, f.calls)
	}
}

func TestRetryStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &flaky{errs: []error{&Error{Kind: ErrTransient, Err: errors.New("x")}}}
	p := RetryPolicy{MaxAttempts: 3, Base: time.Hour, Sleep: func(ctx context.Context, d time.Duration) error { cancel(); return ctx.Err() }}
	_, err := WithRetry(f, p).Generate(ctx, Request{})
	var le *Error
	if !errors.As(err, &le) || le.Kind != ErrCancelled {
		t.Fatalf("%v", err)
	}
}

func TestParseRetryAfter(t *testing.T) {
	if parseRetryAfter("7") != 7*time.Second || parseRetryAfter("") != 0 || parseRetryAfter("garbage") != 0 {
		t.Fatal("retry-after parsing")
	}
}

func TestOpenAIRequiresKeyAndModel(t *testing.T) {
	if _, err := NewOpenAI("", "", "m"); err == nil {
		t.Fatal("missing key must be an error")
	}
	if _, err := NewOpenAI("k", "", ""); err == nil {
		t.Fatal("missing model must be an error")
	}
}
