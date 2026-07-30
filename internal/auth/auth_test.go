package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFreebuffCredentialsFormats(t *testing.T) {
	tests := []struct {
		name string
		json string
		want string
	}{
		{name: "modern", json: `{"default":{"authToken":" modern-token "}}`, want: "modern-token"},
		{name: "legacy", json: `{"authToken":"legacy-token","username":"user@example.com"}`, want: "legacy-token"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := testFreebuffFinder(t, tt.json)
			cred, err := f.FreebuffCredentials()
			if err != nil {
				t.Fatalf("FreebuffCredentials() error = %v", err)
			}
			if cred.Token != tt.want {
				t.Fatalf("Token = %q, want %q", cred.Token, tt.want)
			}
		})
	}
}

func TestFreebuffCredentialsMissingOrEmptyToken(t *testing.T) {
	for _, raw := range []string{`{"default":{}}`, `{"authToken":"   "}`, `{}`} {
		f := testFreebuffFinder(t, raw)
		if _, err := f.FreebuffCredentials(); err == nil {
			t.Fatalf("FreebuffCredentials(%s) returned nil error", raw)
		} else if !strings.Contains(err.Error(), "authToken") {
			t.Fatalf("error = %v, want authToken context", err)
		}
	}
}

func testFreebuffFinder(t *testing.T, body string) *Finder {
	t.Helper()
	root := t.TempDir()
	f := &Finder{Home: root, XDG: filepath.Join(root, "config"), Data: filepath.Join(root, "data")}
	path := f.FreebuffCredentialsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}
