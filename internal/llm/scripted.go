package llm

import (
	"context"
	"fmt"
	"sync"
)

// Scripted is a deterministic Provider for tests: each call returns the next
// scripted step. It records every request it receives.
type Scripted struct {
	mu       sync.Mutex
	Steps    []func(Request) (*Response, error)
	Requests []Request
	n        int
}

func (s *Scripted) Name() string  { return "scripted" }
func (s *Scripted) Model() string { return "scripted-model" }

func (s *Scripted) Generate(ctx context.Context, req Request) (*Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, &Error{Kind: ErrCancelled, Err: err}
	}
	s.Requests = append(s.Requests, req)
	if s.n >= len(s.Steps) {
		return nil, fmt.Errorf("scripted provider: no step %d scripted", s.n)
	}
	step := s.Steps[s.n]
	s.n++
	resp, err := step(req)
	if resp != nil && resp.ID == "" {
		resp.ID = fmt.Sprintf("resp_%d", s.n)
	}
	return resp, err
}

// Reply and Calls build common step shapes.
func Reply(text string) func(Request) (*Response, error) {
	return func(Request) (*Response, error) { return &Response{Text: text}, nil }
}

func Calls(calls ...ToolCall) func(Request) (*Response, error) {
	return func(Request) (*Response, error) { return &Response{ToolCalls: calls}, nil }
}
