package opencodeutil

import (
	"net/http"
	"testing"
	"time"
)

func TestCookieHeaderValue(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "bare browser value", in: "token-123", want: "auth=token-123"},
		{name: "existing auth pair", in: "auth=token-123", want: "auth=token-123"},
		{name: "complete header", in: "auth=token-123; locale=en", want: "auth=token-123; locale=en"},
		{name: "surrounding whitespace", in: "  token-123\n", want: "auth=token-123"},
		{name: "empty", in: "  ", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cookieHeaderValue(tt.in); got != tt.want {
				t.Fatalf("cookieHeaderValue returned %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAttachToRequestNormalizesBareAuthValue(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cache, err := NewCookieCache()
	if err != nil {
		t.Fatalf("NewCookieCache: %v", err)
	}
	if err := cache.Write(&CacheCookie{Source: "browser-import", Cookie: "secret-token", CachedAt: time.Now()}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, "https://opencode.ai/_server", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	cache.AttachToRequest(req)
	if got := req.Header.Get("Cookie"); got != "auth=secret-token" {
		t.Fatalf("Cookie header = %q, want normalized auth pair", got)
	}
}

func TestAttachToRequestEmptyCache(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cache, err := NewCookieCache()
	if err != nil {
		t.Fatalf("NewCookieCache: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, "https://opencode.ai/_server", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	cache.AttachToRequest(req)
	if got := req.Header.Get("Cookie"); got != "" {
		t.Fatalf("Cookie header = %q, want empty", got)
	}
}
