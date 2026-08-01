package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/TheMetalStorm/plan-usage/internal/opencodeutil"
)

// withCookieState points the cookie cache at a temp XDG_STATE_HOME for the
// duration of one test.
func withCookieState(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
}

func TestRunCookieWritesAndRoundTrips(t *testing.T) {
	withCookieState(t)
	var stdout, stderr bytes.Buffer
	if err := runCookie([]string{"secret-cookie"}, &stdout, &stderr); err != nil {
		t.Fatalf("runCookie(write) error = %v", err)
	}
	if !strings.Contains(stdout.String(), "saved") {
		t.Fatalf("write output missing confirmation: %q", stdout.String())
	}

	cc, err := opencodeutil.NewCookieCache()
	if err != nil {
		t.Fatalf("NewCookieCache: %v", err)
	}
	cached, err := cc.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if cached == nil || cached.Cookie != "secret-cookie" || cached.Source != "cli" {
		t.Fatalf("cached = %#v, want Cookie=secret-cookie Source=cli", cached)
	}
}

func TestRunCookieNoArgDoesNotLeak(t *testing.T) {
	withCookieState(t)
	var stdout bytes.Buffer
	if err := runCookie([]string{"super-secret-value"}, &stdout, &bytes.Buffer{}); err != nil {
		t.Fatalf("runCookie(write) error = %v", err)
	}

	stdout.Reset()
	if err := runCookie(nil, &stdout, &bytes.Buffer{}); err != nil {
		t.Fatalf("runCookie(status) error = %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "source=cli") {
		t.Fatalf("status output missing cache state: %q", out)
	}
	if strings.Contains(out, "super-secret-value") {
		t.Fatalf("status output leaked the cookie value: %q", out)
	}
}

func TestRunCookieClear(t *testing.T) {
	withCookieState(t)
	if err := runCookie([]string{"secret"}, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("runCookie(write) error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runCookie([]string{"--clear"}, &stdout, &bytes.Buffer{}); err != nil {
		t.Fatalf("runCookie(clear) error = %v", err)
	}
	if !strings.Contains(stdout.String(), "cleared") {
		t.Fatalf("clear output = %q, want confirmation", stdout.String())
	}

	cc, err := opencodeutil.NewCookieCache()
	if err != nil {
		t.Fatalf("NewCookieCache: %v", err)
	}
	if cc.Cookie() != "" {
		t.Fatalf("cookie still set after --clear: %q", cc.Cookie())
	}
}
