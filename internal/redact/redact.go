// Package redact removes secrets from text before it reaches the model, the
// terminal, or the audit log.
package redact

import (
	"regexp"
	"strings"
	"sync"
)

const Mask = "[REDACTED]"

var patterns = []*regexp.Regexp{
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(-----END [A-Z ]*PRIVATE KEY-----|$)`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{16,}`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9\-]{10,}`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`),
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/\-]{16,}=*`),
	// scheme://user:password@host
	regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://[^/\s:@]+:)[^@\s/]+(@)`),
}

// assignment matches NAME=value / NAME: value / "name": "value" where NAME looks secret.
var assignment = regexp.MustCompile(`(?i)(["']?[A-Za-z0-9_.\-]*(?:secret|token|passwd|password|pwd|api[_\-]?key|apikey|access[_\-]?key|private[_\-]?key|credential|auth)[A-Za-z0-9_.\-]*["']?\s*[:=]\s*)("[^"\n]*"|'[^'\n]*'|[^\s"',;&]+)`)

var secretName = regexp.MustCompile(`(?i)secret|token|passwd|password|pwd|api[_\-]?key|apikey|access[_\-]?key|private[_\-]?key|credential|auth|session|cookie`)

// IsSecretName reports whether a variable or key name suggests a secret value.
func IsSecretName(name string) bool {
	if name == "PWD" || name == "OLDPWD" { // working-directory variables, not passwords
		return false
	}
	return secretName.MatchString(name)
}

// LooksLikeSecretValue decides whether an environment variable's exact value
// should be scrubbed everywhere. It is stricter than IsSecretName: paths,
// short values and session identifiers are far more often ordinary text.
func LooksLikeSecretValue(name, value string) bool {
	return IsSecretName(name) && len(value) >= 12 && !strings.HasPrefix(value, "/") &&
		!strings.Contains(strings.ToUpper(name), "SESSION") && !strings.Contains(strings.ToUpper(name), "SOCK")
}

// Redactor redacts pattern-matched secrets plus any exact values registered at runtime.
type Redactor struct {
	mu      sync.RWMutex
	secrets []string
}

func New(secrets ...string) *Redactor {
	r := &Redactor{}
	for _, s := range secrets {
		r.AddSecret(s)
	}
	return r
}

// AddSecret registers an exact value (e.g. the API key) to scrub. Very short values are ignored.
func (r *Redactor) AddSecret(v string) {
	v = strings.TrimSpace(v)
	if len(v) < 6 {
		return
	}
	r.mu.Lock()
	r.secrets = append(r.secrets, v)
	r.mu.Unlock()
}

func (r *Redactor) Redact(s string) string {
	if r != nil {
		r.mu.RLock()
		for _, sec := range r.secrets {
			s = strings.ReplaceAll(s, sec, Mask)
		}
		r.mu.RUnlock()
	}
	return String(s)
}

// String applies pattern-based redaction only.
func String(s string) string {
	for _, p := range patterns {
		if p.NumSubexp() == 2 {
			s = p.ReplaceAllString(s, "${1}"+Mask+"${2}")
		} else {
			s = p.ReplaceAllString(s, Mask)
		}
	}
	return assignment.ReplaceAllStringFunc(s, func(m string) string {
		loc := assignment.FindStringSubmatchIndex(m)
		if loc == nil {
			return m
		}
		val := m[loc[4]:loc[5]]
		if val == Mask || strings.Trim(val, `"'`) == Mask {
			return m
		}
		return m[:loc[4]] + Mask
	})
}
