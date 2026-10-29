// Package tools defines the capability interface the agent can use and the
// registry that holds those capabilities. Tools never decide whether they are
// allowed to run; that is the policy engine's job, enforced by the executor.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Kind categorises a failure so the agent can react sensibly.
type Kind string

const (
	KindNone         Kind = ""
	KindInvalidInput Kind = "invalid_input"
	KindNotFound     Kind = "not_found"
	KindTimeout      Kind = "timeout"
	KindCancelled    Kind = "cancelled"
	KindDenied       Kind = "denied"
	KindUnavailable  Kind = "unavailable" // executable missing, unsupported platform
	KindFailed       Kind = "failed"      // the operation ran and failed (e.g. nonzero exit)
	KindInternal     Kind = "internal"
)

// Result is the structured outcome of a tool run.
type Result struct {
	Output    string            // text shown to the model (redacted and truncated by the executor)
	ExitCode  int               // process exit code, 0 for non-process tools on success
	Kind      Kind              // KindNone on success
	Truncated bool              // output was cut to the size limit
	Mutated   bool              // the tool changed system or workspace state
	Metadata  map[string]string // duration, paths, backup locations, ...
}

func (r Result) OK() bool { return r.Kind == KindNone && r.ExitCode == 0 }

// Error lets tools return a categorised error.
type Error struct {
	Kind Kind
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

func Errorf(k Kind, format string, a ...any) error {
	return &Error{Kind: k, Msg: fmt.Sprintf(format, a...)}
}

// KindOf extracts the Kind from err (KindInternal for plain errors).
func KindOf(err error) Kind {
	var te *Error
	if errors.As(err, &te) {
		return te.Kind
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return KindTimeout
	case errors.Is(err, context.Canceled):
		return KindCancelled
	}
	return KindInternal
}

type Tool interface {
	Name() string
	Description() string
	InputSchema() json.RawMessage
	// Validate checks arguments without side effects.
	Validate(input json.RawMessage) error
	Execute(ctx context.Context, input json.RawMessage) (Result, error)
}

// Previewer is optionally implemented by tools that can show what they would
// change (a diff) so the user can review it before approving.
type Previewer interface {
	Preview(ctx context.Context, input json.RawMessage) (string, error)
}

// Typed builds a Tool from a typed handler. The input is decoded strictly
// (unknown fields are rejected) and then checked with validate, if given.
type Typed[T any] struct {
	ToolName   string
	Desc       string
	Schema     string
	ValidateFn func(*T) error
	Run        func(ctx context.Context, in T) (Result, error)
	PreviewFn  func(ctx context.Context, in T) (string, error)
}

func (t *Typed[T]) Name() string                 { return t.ToolName }
func (t *Typed[T]) Description() string          { return t.Desc }
func (t *Typed[T]) InputSchema() json.RawMessage { return json.RawMessage(t.Schema) }

func (t *Typed[T]) decode(input json.RawMessage) (T, error) {
	var in T
	if len(bytes.TrimSpace(input)) == 0 {
		input = json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, Errorf(KindInvalidInput, "invalid arguments for %s: %v", t.ToolName, err)
	}
	if dec.More() {
		return in, Errorf(KindInvalidInput, "invalid arguments for %s: trailing data", t.ToolName)
	}
	if t.ValidateFn != nil {
		if err := t.ValidateFn(&in); err != nil {
			return in, Errorf(KindInvalidInput, "invalid arguments for %s: %v", t.ToolName, err)
		}
	}
	return in, nil
}

func (t *Typed[T]) Validate(input json.RawMessage) error { _, err := t.decode(input); return err }

func (t *Typed[T]) Execute(ctx context.Context, input json.RawMessage) (Result, error) {
	in, err := t.decode(input)
	if err != nil {
		return Result{}, err
	}
	return t.Run(ctx, in)
}

func (t *Typed[T]) Preview(ctx context.Context, input json.RawMessage) (string, error) {
	if t.PreviewFn == nil {
		return "", nil
	}
	in, err := t.decode(input)
	if err != nil {
		return "", err
	}
	return t.PreviewFn(ctx, in)
}

// Registry holds tools by name.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

func NewRegistry() *Registry { return &Registry{tools: map[string]Tool{}} }

func (r *Registry) Register(t Tool) error {
	if t.Name() == "" {
		return errors.New("tool has no name")
	}
	if !json.Valid(t.InputSchema()) {
		return fmt.Errorf("tool %s has an invalid input schema", t.Name())
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.tools[t.Name()]; dup {
		return fmt.Errorf("tool %q already registered", t.Name())
	}
	r.tools[t.Name()] = t
	return nil
}

func (r *Registry) MustRegister(ts ...Tool) {
	for _, t := range ts {
		if err := r.Register(t); err != nil {
			panic(err)
		}
	}
}

func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// List returns tools sorted by name.
func (r *Registry) List() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// Grant carries permissions the executor attached after policy approval. Tools
// use it as a second line of defence; they never grant themselves anything.
type Grant struct {
	OutsideWorkspace bool
}

type grantKey struct{}

func WithGrant(ctx context.Context, g Grant) context.Context {
	return context.WithValue(ctx, grantKey{}, g)
}

func GrantFrom(ctx context.Context) Grant {
	g, _ := ctx.Value(grantKey{}).(Grant)
	return g
}
