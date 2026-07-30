// Package auth discovers credentials from native-CLI config files
// (e.g. ~/.codex/auth.json, ~/.local/share/opencode/auth.json) and
// from environment overrides. The user can always supply a manual
// override through Config.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Credential is a single resolved token ready to feed to a Provider.
type Credential struct {
	Token    string // bearer token / api key
	Endpoint string // optional endpoint override (CLI defaults preserved if empty)
	Source   string // human-readable origin for the debug panel
}

// Finder abstracts filesystem paths so tests can inject temp dirs.
type Finder struct {
	Home string // $HOME
	XDG  string // $XDG_CONFIG_HOME || $HOME/.config
	Data string // $XDG_DATA_HOME   || $HOME/.local/share
}

// NewFinder returns a Finder with sensible defaults.
func NewFinder() (*Finder, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		xdg = filepath.Join(home, ".config")
	}
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		data = filepath.Join(home, ".local", "share")
	}
	return &Finder{Home: home, XDG: xdg, Data: data}, nil
}

// CodexPath -- ~/.codex/auth.json
func (f *Finder) CodexPath() string {
	return filepath.Join(f.Home, ".codex", "auth.json")
}

// OpenCodeAuthPath -- $XDG_DATA_HOME/opencode/auth.json
func (f *Finder) OpenCodeAuthPath() string {
	return filepath.Join(f.Data, "opencode", "auth.json")
}

// CommandCodeAuthPath -- ~/.commandcode/auth.json
func (f *Finder) CommandCodeAuthPath() string {
	return filepath.Join(f.Home, ".commandcode", "auth.json")
}

// FreebuffCredentialsPath -- $XDG_CONFIG_HOME/manicode/credentials.json
func (f *Finder) FreebuffCredentialsPath() string {
	return filepath.Join(f.XDG, "manicode", "credentials.json")
}

// ClineConfigPaths -- VS Code global storage + ~/.cline/config.json
func (f *Finder) ClineConfigPaths() []string {
	return []string{
		filepath.Join(f.XDG, "Code", "User", "globalStorage", "saoudrizwan.claude-dev", "settings.json"),
		filepath.Join(f.XDG, "Code", "User", "globalStorage", "roo-cline.roo-cline", "settings.json"),
		filepath.Join(f.Home, ".cline", "config.json"),
		filepath.Join(f.XDG, "cline", "config.json"),
	}
}

// ClineCLIProvidersPaths -- candidate paths for the standalone CLINE CLI's
// providers.json file (written with 0600 perms by the Cline team). The
// first entry honours $CLINE_DATA_DIR; the rest mirror the XDG-aware
// candidates we use for the legacy flat-JSON configs so users on either
// ~/.cline or ~/.config/cline installs are both detected.
func (f *Finder) ClineCLIProvidersPaths() []string {
	paths := []string{}
	if d := strings.TrimSpace(os.Getenv("CLINE_DATA_DIR")); d != "" {
		paths = append(paths, filepath.Join(d, "settings", "providers.json"))
	}
	paths = append(paths,
		filepath.Join(f.Home, ".cline", "data", "settings", "providers.json"),
		filepath.Join(f.Data, "cline", "data", "settings", "providers.json"),
		filepath.Join(f.XDG, "cline", "data", "settings", "providers.json"),
	)
	return paths
}

// readFile returns os.ReadFile wrapped for symmetry.
func (f *Finder) readFile(path string) ([]byte, error) { return os.ReadFile(path) }

// CodexToken reads ~/.codex/auth.json and extracts the bearer / api_key.
//
// Two shapes are supported:
//
//  1. Legacy OPENAI_API_KEY at top level — used by codex pre-0.50 or when
//     the user configured a custom OpenAI key.
//  2. OAuth: when auth_mode == "chatgpt", the `tokens.access_token` JWT
//     is the bearer for chatgpt.com/backend-api/wham/usage.
func (f *Finder) CodexToken() (*Credential, error) {
	raw, err := f.readFile(f.CodexPath())
	if err != nil {
		return nil, fmt.Errorf("codex auth: %w", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("codex auth: parse: %w", err)
	}
	for _, k := range []string{"OPENAI_API_KEY", "api_key", "token"} {
		if v, ok := doc[k].(string); ok && v != "" {
			return &Credential{Token: v, Source: f.CodexPath()}, nil
		}
	}
	if tokens, ok := doc["tokens"].(map[string]any); ok {
		for _, k := range []string{"access_token", "id_token", "refresh_token"} {
			if v, ok := tokens[k].(string); ok && v != "" {
				return &Credential{
					Token:  v,
					Source: f.CodexPath() + " (oauth:" + k + ")",
				}, nil
			}
		}
	}
	return nil, fmt.Errorf("codex auth: token field missing in %s", f.CodexPath())
}

// CodexOAuthToken reads ~/.codex/auth.json and returns the ChatGPT OAuth
// credentials when present. Returns ok=false when the file is missing or
// the user is not on the chatgpt auth_mode (e.g. legacy OPENAI_API_KEY).
func (f *Finder) CodexOAuthToken() (access, refresh string, lastRefresh time.Time, ok bool) {
	raw, err := f.readFile(f.CodexPath())
	if err != nil {
		return "", "", time.Time{}, false
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", "", time.Time{}, false
	}
	if mode, _ := doc["auth_mode"].(string); mode != "chatgpt" {
		return "", "", time.Time{}, false
	}
	tokens, _ := doc["tokens"].(map[string]any)
	if tokens == nil {
		return "", "", time.Time{}, false
	}
	access, _ = tokens["access_token"].(string)
	refresh, _ = tokens["refresh_token"].(string)
	if lr, ok := doc["last_refresh"].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, lr); err == nil {
			lastRefresh = t
		} else if t, err := time.Parse(time.RFC3339, lr); err == nil {
			lastRefresh = t
		}
	}
	if access != "" {
		return access, refresh, lastRefresh, true
	}
	return "", "", time.Time{}, false
}

// CodexAuthMode returns the auth_mode field of ~/.codex/auth.json when
// present, or "" when the file is missing / unparsable. Callers use this
// to decide whether to prefer the codex CLI app-server (api-key legacy
// users) or the OAuth wham/usage path (chatgpt-auth users).
func (f *Finder) CodexAuthMode() string {
	raw, err := f.readFile(f.CodexPath())
	if err != nil {
		return ""
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}
	mode, _ := doc["auth_mode"].(string)
	return mode
}

// OpenCodeAuth returns the full opencode auth.json (map of provider -> blob).
func (f *Finder) OpenCodeAuth() (map[string]json.RawMessage, string, error) {
	raw, err := f.readFile(f.OpenCodeAuthPath())
	if err != nil {
		return nil, "", fmt.Errorf("opencode auth: %w", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, "", fmt.Errorf("opencode auth: parse: %w", err)
	}
	return m, f.OpenCodeAuthPath(), nil
}

// CommandCodeToken prefers the env var, falls back to the auth file.
func (f *Finder) CommandCodeToken() (*Credential, error) {
	if env := os.Getenv("COMMAND_CODE_API_KEY"); env != "" {
		return &Credential{Token: env, Source: "env:COMMAND_CODE_API_KEY"}, nil
	}
	raw, err := f.readFile(f.CommandCodeAuthPath())
	if err != nil {
		return nil, fmt.Errorf("commandcode auth: %w", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("commandcode auth: parse: %w", err)
	}
	for _, k := range []string{"api_key", "apiKey", "token", "access_token"} {
		if v, ok := doc[k].(string); ok && v != "" {
			return &Credential{Token: v, Source: f.CommandCodeAuthPath()}, nil
		}
	}
	return nil, errors.New("commandcode auth: no api_key in file")
}

// FreebuffCredentials reads the manicode credentials file.
func (f *Finder) FreebuffCredentials() (*Credential, error) {
	raw, err := f.readFile(f.FreebuffCredentialsPath())
	if err != nil {
		return nil, fmt.Errorf("freebuff credentials: %w", err)
	}
	var doc struct {
		AuthToken string `json:"authToken"`
		Username  string `json:"username"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("freebuff credentials: parse: %w", err)
	}
	if doc.AuthToken == "" {
		return nil, errors.New("freebuff credentials: missing authToken")
	}
	return &Credential{Token: doc.AuthToken, Source: f.FreebuffCredentialsPath()}, nil
}

// ClinePassCredentials locates a Cline Pass bearer token by checking, in
// order:
//
//  1. The $CLINE_API_KEY environment variable (developer convenience, mirrors
//     the CommandCode / Freebuff convention).
//  2. The standalone CLINE CLI's providers.json file, written with 0600
//     permissions under $CLINE_DATA_DIR/settings/providers.json (default:
//     ~/.cline/data/settings/providers.json). The CLI stores a map of
//     provider-name -> { apiKey | accessToken | token, expiresAt? }.
//  3. The legacy "VS Code extension + flat JSON" paths handled by
//     clineScan() so users on older setups keep working.
func (f *Finder) ClinePassCredentials() (*Credential, error) {
	if env := strings.TrimSpace(os.Getenv("CLINE_API_KEY")); env != "" {
		return &Credential{Token: env, Source: "env:CLINE_API_KEY"}, nil
	}
	if cred, ok := f.scanClineCLIProviders(); ok {
		return cred, nil
	}
	for _, p := range f.ClineConfigPaths() {
		raw, err := f.readFile(p)
		if err != nil {
			continue
		}
		if cred := clineScan(raw, p); cred != nil {
			return cred, nil
		}
	}
	return nil, errors.New("cline pass: no credentials found in env, the standalone CLINE CLI (run `cline auth` / `cline login`), or any legacy VS Code / ~/.cline config path")
}

// clineProviderKeys are the providers.json entries we treat as the
// "Cline Pass" subscription. The Cline CLI has shipped a few spellings;
// we try all of them but the runtime order is driven by the top-level
// `lastUsedProvider` field when it identifies a recognised provider.
var clineProviderKeys = []string{
	"cline-pass",
	"clinePass",
	"cline_pass",
	"cline",
}

// clineCLIProvidersDoc mirrors the shape the standalone Cline CLI v3.x
// writes to providers.json (deeply nested:
//
//	providers.<key>.settings.auth.{accessToken, refreshToken, expiresAt}
//	providers.<key>.settings.model
//	lastUsedProvider
//
// Older CLINE-CLI versions wrote a flat shape with apiKey/token at the
// top level; those are still handled by clineScan() in the legacy
// fallback chain. The two parsers are non-overlapping by design.
type clineCLIProvidersDoc struct {
	Version          int                              `json:"version"`
	LastUsedProvider string                           `json:"lastUsedProvider"`
	Providers        map[string]clineCLIProviderEntry `json:"providers"`
}

type clineCLIProviderEntry struct {
	Settings    clineCLISettings `json:"settings"`
	TokenSource string           `json:"tokenSource"`
	UpdatedAt   string           `json:"updatedAt"`
}

type clineCLISettings struct {
	Provider string       `json:"provider"`
	Auth     clineCLIAuth `json:"auth"`
	Model    string       `json:"model"`
}

// clineCLIAuth is the auth block inside one provider entry.
//
// NB: the accessToken here is a WorkOS session JWT (prefix "workos:...")
// and is NOT directly accepted as a Bearer against api.cline.bot/api/v1/.
// We still surface it so callers can recognise the user as signed-in,
// but the provider layer is responsible for not blindly forwarding it to
// /api/v1/chat/completions -- that needs a separate Cline Pass API key.
type clineCLIAuth struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	// expiresAt arrived as a Unix-millis NUMBER in CLI v3.x but as an
	// RFC3339 string in v2.x. Use json.RawMessage and accept both in
	// parseClineExpiry so neither shape silently maps to "never expires".
	ExpiresAt json.RawMessage `json:"expiresAt"`
	AccountID string          `json:"accountId"`
}

// scanClineCLIProviders walks every candidate providers.json path and
// returns the first matching entry whose OAuth access token is still
// unexpired. Returns ok=false when no file exists / parses, or when no
// entry has a usable token -- callers fall through to the legacy
// clineScan() chain for older / flat-shape config files.
func (f *Finder) scanClineCLIProviders() (*Credential, bool) {
	for _, path := range f.ClineCLIProvidersPaths() {
		raw, err := f.readFile(path)
		if err != nil {
			continue
		}
		var doc clineCLIProvidersDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			continue
		}
		for _, key := range orderedClineProviderKeys(doc.LastUsedProvider) {
			entry, ok := doc.Providers[key]
			if !ok {
				continue
			}
			cred, ok := clineAuthToCredential(entry.Settings.Auth, path+" (#"+key+")")
			if ok {
				return cred, true
			}
		}
	}
	return nil, false
}

// orderedClineProviderKeys returns the priority list of provider keys to
// probe: the top-level lastUsedProvider first (when it's one we
// recognise), then the rest in our static fallback order.
func orderedClineProviderKeys(lastUsed string) []string {
	out := make([]string, 0, len(clineProviderKeys)+1)
	if lastUsed != "" {
		for _, k := range clineProviderKeys {
			if k == lastUsed {
				out = append(out, k)
				break
			}
		}
	}
	for _, k := range clineProviderKeys {
		seen := false
		for _, e := range out {
			if e == k {
				seen = true
				break
			}
		}
		if !seen {
			out = append(out, k)
		}
	}
	return out
}

// clineAuthToCredential lifts the OAuth accessToken out of one provider
// entry into our Credential shape. An empty accessToken or a stale
// expiresAt (timestamp earlier than now) returns false so the caller
// keeps scanning; the Cline CLI refreshes these on `cline auth` / at
// next launch.
func clineAuthToCredential(a clineCLIAuth, src string) (*Credential, bool) {
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, false
	}
	if len(a.ExpiresAt) > 0 {
		if t, ok := parseClineExpiry(a.ExpiresAt); ok {
			if t.Before(time.Now()) {
				return nil, false
			}
			src = src + ", expiresAt=" + string(a.ExpiresAt)
		}
	}
	return &Credential{Token: a.AccessToken, Source: src}, true
}

// parseClineExpiry accepts a JSON-encoded value. It supports:
//   - numeric Unix millis (>= 1e12), what CLI v3 writes;
//   - numeric Unix seconds (< 1e12), older epochs;
//   - RFC3339 / RFC3339Nano strings, what CLI v2 wrote.
//
// Returns (time.Time{}, false) on empty or unparseable input.
func parseClineExpiry(raw json.RawMessage) (time.Time, bool) {
	if len(raw) == 0 {
		return time.Time{}, false
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		if n <= 0 {
			return time.Time{}, false
		}
		if n >= 1_000_000_000_000 {
			return time.UnixMilli(n), true
		}
		return time.Unix(n, 0), true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if t, err := time.Parse(layout, s); err == nil {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

// clineScan extracts an api key from a Cline config blob. Cline has evolved
// over time, so we test a few known shapes.
func clineScan(raw []byte, src string) *Credential {
	var loose struct {
		APIKey string `json:"api_key"`
		Token  string `json:"token"`
		Cline  struct {
			APIKey string `json:"apiKey"`
		} `json:"cline"`
	}
	if err := json.Unmarshal(raw, &loose); err == nil {
		if t := firstNonEmpty(loose.APIKey, loose.Token, loose.Cline.APIKey); t != "" {
			return &Credential{Token: t, Source: src}
		}
	}
	// Fallback: VS Code settings-style "cline-pass.apiKey" / "clinePass.apiKey".
	for _, key := range []string{`"cline-pass.apiKey"`, `"clinePass.apiKey"`, `"apiKey"`} {
		if i := strings.Index(string(raw), key); i >= 0 {
			rest := string(raw)[i+len(key):]
			if j := strings.Index(rest, ":"); j >= 0 {
				rest = rest[j+1:]
				if k := strings.Index(rest, `"`); k >= 0 {
					rest = rest[k+1:]
					if l := strings.Index(rest, `"`); l >= 0 {
						return &Credential{Token: rest[:l], Source: src}
					}
				}
			}
		}
	}
	return nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
