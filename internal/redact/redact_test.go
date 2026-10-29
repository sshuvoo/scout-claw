package redact

import (
	"strings"
	"testing"
)

func TestRedactPatterns(t *testing.T) {
	cases := map[string]string{
		"key sk-abcdefghijklmnop1234 end":                                        "sk-abcdefghij",
		"OPENAI_API_KEY=supersecretvalue123":                                     "supersecretvalue123",
		"password: hunter2hunter2":                                               "hunter2hunter2",
		`{"access_token": "abc123def456"}`:                                       "abc123def456",
		"postgres://admin:s3cr3tpw@db.local:5432/x":                              "s3cr3tpw",
		"Authorization: Bearer abcdefghijklmnopqrst":                             "abcdefghijklmnopqrst",
		"ghp_abcdefghijklmnopqrstuvwxyz0123":                                     "ghp_abcdefghij",
		"AKIAABCDEFGHIJKLMNOP":                                                   "AKIAABCDEFGHIJKLMNOP",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIabc\n-----END RSA PRIVATE KEY-----": "MIIabc",
	}
	for in, leaked := range cases {
		out := String(in)
		if strings.Contains(out, leaked) {
			t.Errorf("secret %q leaked in %q", leaked, out)
		}
		if !strings.Contains(out, Mask) {
			t.Errorf("no mask in %q", out)
		}
	}
}

func TestRedactKeepsOrdinaryText(t *testing.T) {
	in := "listening on port 8080, status ok"
	if got := String(in); got != in {
		t.Fatalf("changed ordinary text: %q", got)
	}
}

func TestRedactorExactValues(t *testing.T) {
	r := New("my-plain-api-key-value")
	if out := r.Redact("x my-plain-api-key-value y"); strings.Contains(out, "plain-api") {
		t.Fatalf("leaked: %q", out)
	}
	r.AddSecret("abc") // too short, ignored
	if out := r.Redact("abc"); out != "abc" {
		t.Fatalf("short secret should be ignored, got %q", out)
	}
}

func TestIsSecretName(t *testing.T) {
	for _, n := range []string{"OPENAI_API_KEY", "DB_PASSWORD", "github_token"} {
		if !IsSecretName(n) {
			t.Errorf("%s should be secret", n)
		}
	}
	if IsSecretName("PATH") || IsSecretName("HOME") {
		t.Error("PATH/HOME are not secrets")
	}
}

func TestPathLikeAndWorkingDirVarsAreNotSecrets(t *testing.T) {
	if IsSecretName("PWD") || IsSecretName("OLDPWD") {
		t.Fatal("PWD is not a secret")
	}
	if LooksLikeSecretValue("SSH_AUTH_SOCK", "/run/user/1000/keyring/ssh") || LooksLikeSecretValue("API_TOKEN", "short") {
		t.Fatal("paths and short values must not be registered as exact secrets")
	}
	if !LooksLikeSecretValue("GITHUB_TOKEN", "ghp_abcdefghijklmnopqrstuvwx") {
		t.Fatal("real tokens must be registered")
	}
}
