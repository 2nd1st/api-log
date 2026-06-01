package main

import (
	"net/http"
	"testing"
)

// TestClientAddrOf exercises the smart XFF chain walker across the
// reverse-proxy topologies api-log is expected to run behind:
// direct, single-hop nginx/Caddy, docker-proxy-on-loopback,
// Cloudflare (IPv4 + IPv6), and Akamai. Each case names the
// real-world deployment it models.
func TestClientAddrOf(t *testing.T) {
	cases := []struct {
		name       string
		headers    map[string]string
		remoteAddr string
		want       string
	}{
		{
			name:       "empty headers falls to RemoteAddr verbatim",
			headers:    nil,
			remoteAddr: "192.0.2.5:54321",
			want:       "192.0.2.5:54321",
		},
		{
			name: "XFF single public IP wins",
			headers: map[string]string{
				"X-Forwarded-For": "8.8.8.8",
			},
			remoteAddr: "127.0.0.1:1234",
			want:       "8.8.8.8",
		},
		{
			name: "XFF only-loopback falls through to next layer (X-Real-IP)",
			headers: map[string]string{
				"X-Forwarded-For": "127.0.0.1",
				"X-Real-IP":       "1.1.1.1",
			},
			remoteAddr: "127.0.0.1:1234",
			want:       "1.1.1.1",
		},
		{
			name: "XFF leftmost private skipped, second public wins",
			headers: map[string]string{
				"X-Forwarded-For": "10.0.0.5, 8.8.8.8",
			},
			remoteAddr: "127.0.0.1:1234",
			want:       "8.8.8.8",
		},
		{
			name: "XFF leftmost public wins over later private",
			headers: map[string]string{
				"X-Forwarded-For": "8.8.8.8, 10.0.0.5",
			},
			remoteAddr: "127.0.0.1:1234",
			want:       "8.8.8.8",
		},
		{
			name: "XFF CGNAT then public — CGNAT skipped, public returned",
			headers: map[string]string{
				"X-Forwarded-For": "100.100.0.1, 8.8.8.8",
			},
			remoteAddr: "127.0.0.1:1234",
			want:       "8.8.8.8",
		},
		{
			name: "XFF empty + X-Real-IP private skipped, fall to next",
			headers: map[string]string{
				"X-Real-IP":        "192.168.1.10",
				"Cf-Connecting-Ip": "2.2.2.2",
			},
			remoteAddr: "127.0.0.1:1234",
			want:       "2.2.2.2",
		},
		{
			name: "X-Real-IP public wins (nginx single-hop)",
			headers: map[string]string{
				"X-Real-IP": "1.1.1.1",
			},
			remoteAddr: "127.0.0.1:1234",
			want:       "1.1.1.1",
		},
		{
			name: "X-Real-IP loopback skipped, Cf-Connecting-Ip wins",
			headers: map[string]string{
				"X-Real-IP":        "127.0.0.1",
				"Cf-Connecting-Ip": "2.2.2.2",
			},
			remoteAddr: "127.0.0.1:1234",
			want:       "2.2.2.2",
		},
		{
			// Representative Cloudflare-fronted case: X-Real-IP can be a
			// CF edge POP while Cf-Connecting-Ip is the real client.
			// Cf-Connecting-Ip MUST win over a non-private X-Real-IP
			// because the X-Real-IP value, while parseable as public, is
			// the CDN's view of the client (CDN edge), not the real
			// client. CF protects Cf-Connecting-Ip from client-side spoof.
			name: "Cloudflare wins over X-Real-IP (CF edge IP is public but not the real client)",
			headers: map[string]string{
				"X-Real-IP":        "172.70.47.73",
				"Cf-Connecting-Ip": "240e:37c:1e41:4600:2806:b427:7a65:4ce3",
			},
			remoteAddr: "127.0.0.1:1234",
			want:       "240e:37c:1e41:4600:2806:b427:7a65:4ce3",
		},
		{
			name: "True-Client-Ip wins over X-Real-IP (Akamai / CF Enterprise convention)",
			headers: map[string]string{
				"X-Real-IP":      "172.70.47.73",
				"True-Client-Ip": "3.3.3.3",
			},
			remoteAddr: "127.0.0.1:1234",
			want:       "3.3.3.3",
		},
		{
			name: "Cf-Connecting-Ip IPv6 returned verbatim",
			headers: map[string]string{
				"Cf-Connecting-Ip": "240e:37c::1",
			},
			remoteAddr: "127.0.0.1:1234",
			want:       "240e:37c::1",
		},
		{
			name: "True-Client-Ip falls last before RemoteAddr",
			headers: map[string]string{
				"True-Client-Ip": "3.3.3.3",
			},
			remoteAddr: "127.0.0.1:1234",
			want:       "3.3.3.3",
		},
		{
			name:       "all headers empty returns RemoteAddr verbatim including port",
			headers:    nil,
			remoteAddr: "127.0.0.1:54321",
			want:       "127.0.0.1:54321",
		},
		{
			name: "XFF garbage skipped, falls to next",
			headers: map[string]string{
				"X-Forwarded-For": "not-an-ip",
				"X-Real-IP":       "1.1.1.1",
			},
			remoteAddr: "127.0.0.1:1234",
			want:       "1.1.1.1",
		},
		{
			name: "XFF IPv4-with-port — port-stripping classifies it correctly",
			headers: map[string]string{
				"X-Forwarded-For": "1.2.3.4:5678",
			},
			remoteAddr: "127.0.0.1:1234",
			want:       "1.2.3.4:5678",
		},
		{
			name: "XFF bracketed IPv6 with port — handled as non-private",
			headers: map[string]string{
				"X-Forwarded-For": "[2001:db8::1]:443",
			},
			remoteAddr: "127.0.0.1:1234",
			want:       "[2001:db8::1]:443",
		},
		{
			name: "XFF private chain only — falls all the way to RemoteAddr",
			headers: map[string]string{
				"X-Forwarded-For": "10.0.0.1, 192.168.1.1, 127.0.0.1",
			},
			remoteAddr: "127.0.0.1:1234",
			want:       "127.0.0.1:1234",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			r, err := http.NewRequest(http.MethodGet, "http://example/", nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			r.RemoteAddr = tc.remoteAddr
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			got := clientAddrOf(r)
			if got != tc.want {
				t.Fatalf("clientAddrOf = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestIsPrivateOrLoopback covers the IP-class classifier directly so
// regressions in the helper surface as helper-level test failures even
// when clientAddrOf wrapping happens to mask them.
func TestIsPrivateOrLoopback(t *testing.T) {
	cases := []struct {
		in   string
		want bool
		why  string
	}{
		{"127.0.0.1", true, "loopback v4"},
		{"::1", true, "loopback v6"},
		{"10.0.0.5", true, "RFC1918 10/8"},
		{"172.16.0.1", true, "RFC1918 172.16/12"},
		{"172.31.255.255", true, "RFC1918 172.16/12 upper edge"},
		{"192.168.1.1", true, "RFC1918 192.168/16"},
		{"fc00::1", true, "RFC4193 unique local"},
		{"fd12::1", true, "RFC4193 unique local"},
		{"169.254.1.1", true, "link-local v4"},
		{"fe80::1", true, "link-local v6"},
		{"100.64.0.1", true, "RFC6598 CGNAT lower edge"},
		{"100.127.255.255", true, "RFC6598 CGNAT upper edge"},
		{"8.8.8.8", false, "public v4"},
		{"1.1.1.1", false, "public v4"},
		{"240e:37c::1", false, "public v6"},
		{"2001:db8::1", false, "documentation v6 (treat as public — net stdlib doesn't classify)"},
		{"not-an-ip", true, "unparseable garbage skipped"},
		{"1.2.3.4:5678", false, "IPv4 with port stripped then classified"},
		{"[2001:db8::1]:443", false, "bracketed IPv6 with port stripped then classified"},
		{"100.63.255.255", false, "just-below CGNAT — still public"},
		{"100.128.0.0", false, "just-above CGNAT — still public"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			got := isPrivateOrLoopback(tc.in)
			if got != tc.want {
				t.Fatalf("isPrivateOrLoopback(%q) = %v, want %v (%s)",
					tc.in, got, tc.want, tc.why)
			}
		})
	}
}
