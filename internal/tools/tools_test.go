package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type echoIn struct {
	Msg string `json:"msg"`
}

func echo(name string) Tool {
	return &Typed[echoIn]{
		ToolName: name, Desc: "echo", Schema: `{"type":"object"}`,
		ValidateFn: func(a *echoIn) error {
			if a.Msg == "" {
				return errors.New("msg is required")
			}
			return nil
		},
		Run: func(ctx context.Context, a echoIn) (Result, error) { return Result{Output: a.Msg}, nil },
	}
}

func TestRegistryRegisterGetListAndDuplicates(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(echo("b"), echo("a"))
	if err := r.Register(echo("a")); err == nil {
		t.Fatal("duplicate name must be rejected")
	}
	if _, ok := r.Get("a"); !ok {
		t.Fatal("lookup failed")
	}
	if _, ok := r.Get("zzz"); ok {
		t.Fatal("unknown tool found")
	}
	l := r.List()
	if len(l) != 2 || l[0].Name() != "a" || l[1].Name() != "b" {
		t.Fatalf("list not sorted: %v", l)
	}
	bad := &Typed[echoIn]{ToolName: "bad", Schema: `{not json`}
	if err := r.Register(bad); err == nil {
		t.Fatal("invalid schema must be rejected")
	}
}

func TestTypedRejectsMalformedArguments(t *testing.T) {
	tool := echo("e")
	cases := map[string]string{
		"not json":      `{`,
		"unknown field": `{"msg":"x","extra":1}`,
		"wrong type":    `{"msg":5}`,
		"missing":       `{}`,
		"trailing":      `{"msg":"x"} {"msg":"y"}`,
	}
	for name, in := range cases {
		err := tool.Validate(json.RawMessage(in))
		if err == nil {
			t.Errorf("%s: expected error", name)
			continue
		}
		if KindOf(err) != KindInvalidInput {
			t.Errorf("%s: kind=%s", name, KindOf(err))
		}
		if _, err := tool.Execute(context.Background(), json.RawMessage(in)); err == nil {
			t.Errorf("%s: Execute must validate too", name)
		}
	}
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"msg":"hi"}`))
	if err != nil || res.Output != "hi" || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestKindOf(t *testing.T) {
	if KindOf(context.DeadlineExceeded) != KindTimeout || KindOf(context.Canceled) != KindCancelled || KindOf(errors.New("x")) != KindInternal {
		t.Fatal("kind mapping wrong")
	}
}
