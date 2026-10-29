package network

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"scoutclaw/internal/tools"
)

func get(name string) tools.Tool {
	for _, t := range Tools() {
		if t.Name() == name {
			return t
		}
	}
	return nil
}

func run(t *testing.T, name string, in map[string]any) tools.Result {
	b, _ := json.Marshal(in)
	r, err := get(name).Execute(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestHTTPStatusRedirectAndFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redir":
			http.Redirect(w, r, "/ok", http.StatusFound)
		case "/ok":
			w.Header().Set("Server", "test")
			w.Write([]byte("hello body"))
		default:
			http.Error(w, "nope", 503)
		}
	}))
	defer srv.Close()

	r := run(t, "net_http", map[string]any{"url": srv.URL + "/redir", "include_body": true})
	if !r.OK() || !strings.Contains(r.Output, "200 OK") || !strings.Contains(r.Output, "redirects:") || !strings.Contains(r.Output, "hello body") {
		t.Fatalf("%+v", r)
	}
	r = run(t, "net_http", map[string]any{"url": srv.URL + "/bad", "method": "HEAD"})
	if r.OK() || !strings.Contains(r.Output, "503") {
		t.Fatalf("HTTP 5xx must be reported as a failure: %+v", r)
	}
}

func TestHTTPValidation(t *testing.T) {
	for _, in := range []string{`{"url":"ftp://x"}`, `{"url":"http://u:p@host/"}`, `{"url":"http://h","method":"POST"}`, `{"url":"/relative"}`} {
		if get("net_http").Validate(json.RawMessage(in)) == nil {
			t.Errorf("%s should be rejected", in)
		}
	}
}

func TestConnect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback")
	}
	port := ln.Addr().(*net.TCPAddr).Port
	r := run(t, "net_connect", map[string]any{"host": "127.0.0.1", "port": port})
	if !r.OK() || !strings.Contains(r.Output, "succeeded") {
		t.Fatalf("%+v", r)
	}
	ln.Close()
	r = run(t, "net_connect", map[string]any{"host": "127.0.0.1", "port": port, "timeout_sec": 2})
	if r.OK() || !strings.Contains(r.Output, "failed") {
		t.Fatalf("closed port must fail: %+v", r)
	}
	if get("net_connect").Validate(json.RawMessage(`{"host":"x","port":`+strconv.Itoa(70000)+`}`)) == nil {
		t.Fatal("bad port accepted")
	}
}

func TestDNS(t *testing.T) {
	r := run(t, "net_dns", map[string]any{"host": "localhost"})
	if !r.OK() || !strings.Contains(r.Output, "127.0.0.1") && !strings.Contains(r.Output, "::1") {
		t.Fatalf("%+v", r)
	}
	r = run(t, "net_dns", map[string]any{"host": "no-such-host.invalid"})
	if r.OK() {
		t.Fatalf("NXDOMAIN must be a failure: %+v", r)
	}
}
