package ids

import (
	"net/http"
	"testing"
)

func TestNewTraceIDLexicallySortable(t *testing.T) {
	// Two ULIDs minted close together should sort: earlier ≤ later as strings.
	a := NewTraceID()
	b := NewTraceID()
	if !(a <= b) {
		t.Errorf("ULIDs not lexically sortable: a=%s b=%s", a, b)
	}
	if len(a) != 26 || len(b) != 26 {
		t.Errorf("ULID string length unexpected: a=%d b=%d", len(a), len(b))
	}
}

func TestKeyHashFromHeaders(t *testing.T) {
	tests := []struct {
		name    string
		headers http.Header
		want    string // first 8 chars only — full value is deterministic from input
		empty   bool
	}{
		{
			name:    "no auth header",
			headers: http.Header{},
			want:    AllZeroKeyHash,
		},
		{
			name:    "authorization wins over x-api-key",
			headers: http.Header{"Authorization": {"Bearer sk-test"}, "X-Api-Key": {"sk-other"}},
		},
		{
			name:    "x-api-key fallback",
			headers: http.Header{"X-Api-Key": {"sk-anth"}},
		},
		{
			name:    "empty authorization treated as absent",
			headers: http.Header{"Authorization": {""}},
			want:    AllZeroKeyHash,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := KeyHashFromHeaders(tc.headers)
			if len(got) != KeyHashLen {
				t.Errorf("hash length = %d, want %d", len(got), KeyHashLen)
			}
			if tc.want != "" && got != tc.want {
				t.Errorf("hash = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestKeyHashDeterministic(t *testing.T) {
	h := http.Header{"Authorization": {"Bearer sk-12345"}}
	a := KeyHashFromHeaders(h)
	b := KeyHashFromHeaders(h)
	if a != b {
		t.Errorf("hash not deterministic: a=%s b=%s", a, b)
	}
}

func TestKeyHashAuthorizationWinsOverXAPIKey(t *testing.T) {
	hAuth := http.Header{"Authorization": {"Bearer A"}, "X-Api-Key": {"B"}}
	hXOnly := http.Header{"X-Api-Key": {"B"}}
	if KeyHashFromHeaders(hAuth) == KeyHashFromHeaders(hXOnly) {
		t.Errorf("Authorization should win over x-api-key, but hashes match")
	}
}

func TestKeyHashShort(t *testing.T) {
	got := KeyHashShort("a1b2c3d4e5f6a7b8")
	if got != "a1b2c3d4" {
		t.Errorf("short = %q, want a1b2c3d4", got)
	}
	if KeyHashShort(AllZeroKeyHash) != "00000000" {
		t.Errorf("short of zero hash unexpected: %q", KeyHashShort(AllZeroKeyHash))
	}
}
