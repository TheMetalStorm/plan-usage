package types

import "testing"

func TestLooksLikeNetworkError(t *testing.T) {
	cases := []struct {
		name string
		err  string
		want bool
	}{
		{"empty", "", false},
		{"io timeout", `Get "...": dial tcp: i/o timeout`, true},
		{"deadline", "context deadline exceeded", true},
		{"refused", "dial tcp: connect: connection refused", true},
		{"dns", `dial tcp: lookup example.com: no such host`, true},
		{"unreachable", "network is unreachable", true},
		{"reset", "read: connection reset by peer", true},
		{"client timeout", "Client.Timeout exceeded while awaiting headers", true},
		{"http401", "freebuff session: HTTP 401: unauthorized", false},
		{"http403", "freebuff session: HTTP 403: forbidden", false},
		{"parse", "freebuff session: parse: invalid character", false},
		{"auth", "not authenticated", false},
		{"missing quota", "missing rateLimitsByModel quota for full access tier", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LooksLikeNetworkError(tc.err); got != tc.want {
				t.Fatalf("LooksLikeNetworkError(%q) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
