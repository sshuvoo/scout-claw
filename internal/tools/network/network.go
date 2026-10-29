// Package network provides bounded connectivity diagnostics.
package network

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"scoutclaw/internal/tools"
)

type dnsArgs struct {
	Host string `json:"host"`
}
type connectArgs struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	TimeoutSec int    `json:"timeout_sec"`
}
type httpArgs struct {
	URL         string `json:"url"`
	Method      string `json:"method"`
	TimeoutSec  int    `json:"timeout_sec"`
	IncludeBody bool   `json:"include_body"`
}

const maxTimeout = 30

func clampTimeout(sec, def int) time.Duration {
	if sec <= 0 {
		sec = def
	}
	return time.Duration(min(sec, maxTimeout)) * time.Second
}

func validHost(h string) error {
	h = strings.TrimSpace(h)
	if h == "" {
		return errors.New("host is required")
	}
	if strings.ContainsAny(h, " /\\\t\n") || len(h) > 253 {
		return errors.New("host is not a valid hostname or IP")
	}
	return nil
}

func Tools() []tools.Tool {
	return []tools.Tool{
		&tools.Typed[dnsArgs]{
			ToolName:   "net_dns",
			Desc:       "Resolve a hostname (A/AAAA, CNAME) using the system resolver, 5s timeout.",
			Schema:     `{"type":"object","properties":{"host":{"type":"string"}},"required":["host"],"additionalProperties":false}`,
			ValidateFn: func(a *dnsArgs) error { return validHost(a.Host) },
			Run: func(ctx context.Context, a dnsArgs) (tools.Result, error) {
				cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				start := time.Now()
				addrs, err := net.DefaultResolver.LookupHost(cctx, a.Host)
				if err != nil {
					return tools.Result{Output: fmt.Sprintf("DNS lookup for %s failed after %s: %v\n", a.Host, time.Since(start).Round(time.Millisecond), err), Kind: tools.KindFailed, ExitCode: 1}, nil
				}
				out := fmt.Sprintf("%s resolves to %s (%s)\n", a.Host, strings.Join(addrs, ", "), time.Since(start).Round(time.Millisecond))
				if cname, err := net.DefaultResolver.LookupCNAME(cctx, a.Host); err == nil && cname != "" && !strings.EqualFold(strings.TrimSuffix(cname, "."), a.Host) {
					out += "canonical name: " + cname + "\n"
				}
				return tools.Result{Output: out}, nil
			},
		},
		&tools.Typed[connectArgs]{
			ToolName: "net_connect",
			Desc:     "Test whether a TCP connection to host:port can be established (default timeout 5s, max 30s).",
			Schema:   `{"type":"object","properties":{"host":{"type":"string"},"port":{"type":"integer","minimum":1,"maximum":65535},"timeout_sec":{"type":"integer"}},"required":["host","port"],"additionalProperties":false}`,
			ValidateFn: func(a *connectArgs) error {
				if a.Port < 1 || a.Port > 65535 {
					return errors.New("port must be 1-65535")
				}
				return validHost(a.Host)
			},
			Run: func(ctx context.Context, a connectArgs) (tools.Result, error) {
				addr := net.JoinHostPort(a.Host, strconv.Itoa(a.Port))
				d := net.Dialer{Timeout: clampTimeout(a.TimeoutSec, 5)}
				start := time.Now()
				conn, err := d.DialContext(ctx, "tcp", addr)
				if err != nil {
					return tools.Result{Output: fmt.Sprintf("TCP connect to %s failed after %s: %v\n", addr, time.Since(start).Round(time.Millisecond), err), Kind: tools.KindFailed, ExitCode: 1}, nil
				}
				defer conn.Close()
				return tools.Result{Output: fmt.Sprintf("TCP connect to %s succeeded in %s (remote %s)\n", addr, time.Since(start).Round(time.Millisecond), conn.RemoteAddr())}, nil
			},
		},
		&tools.Typed[httpArgs]{
			ToolName: "net_http",
			Desc:     "Send an HTTP GET or HEAD request (no credentials, max 5 redirects) and report status, selected headers, timing and optionally the first 2KB of body.",
			Schema:   `{"type":"object","properties":{"url":{"type":"string"},"method":{"type":"string","enum":["GET","HEAD"]},"timeout_sec":{"type":"integer"},"include_body":{"type":"boolean"}},"required":["url"],"additionalProperties":false}`,
			ValidateFn: func(a *httpArgs) error {
				u, err := url.Parse(a.URL)
				if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
					return errors.New("url must be an absolute http(s) URL")
				}
				if u.User != nil {
					return errors.New("credentials in URLs are not allowed")
				}
				switch strings.ToUpper(a.Method) {
				case "", "GET", "HEAD":
				default:
					return errors.New("method must be GET or HEAD")
				}
				return nil
			},
			Run: runHTTP,
		},
	}
}

func runHTTP(ctx context.Context, a httpArgs) (tools.Result, error) {
	method := strings.ToUpper(a.Method)
	if method == "" {
		method = "GET"
	}
	timeout := clampTimeout(a.TimeoutSec, 10)
	var hops []string
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("stopped after 5 redirects")
			}
			hops = append(hops, req.URL.String())
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, method, a.URL, nil)
	if err != nil {
		return tools.Result{}, tools.Errorf(tools.KindInvalidInput, "%v", err)
	}
	req.Header.Set("User-Agent", "scoutclaw-diagnostics")
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return tools.Result{Output: fmt.Sprintf("%s %s failed after %s: %v\n", method, a.URL, time.Since(start).Round(time.Millisecond), err), Kind: tools.KindFailed, ExitCode: 1}, nil
	}
	defer resp.Body.Close()
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s -> %s in %s\n", method, a.URL, resp.Status, time.Since(start).Round(time.Millisecond))
	if len(hops) > 0 {
		fmt.Fprintf(&b, "redirects: %s\n", strings.Join(hops, " -> "))
	}
	for _, h := range []string{"Content-Type", "Content-Length", "Server", "Location", "Date", "Retry-After", "WWW-Authenticate"} {
		if v := resp.Header.Get(h); v != "" {
			fmt.Fprintf(&b, "%s: %s\n", h, v)
		}
	}
	if a.IncludeBody && method == "GET" {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		fmt.Fprintf(&b, "\nbody (first %d bytes, untrusted data):\n%s\n", len(body), body)
	}
	res := tools.Result{Output: b.String()}
	if resp.StatusCode >= 400 {
		res.Kind, res.ExitCode = tools.KindFailed, 1
	}
	return res, nil
}
