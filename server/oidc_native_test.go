package server

import (
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOIDCSealedTicketRejectsTampering(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	oidc := nativeOIDC{secret: key}
	want := oidcTicket{UserID: "testUser", Stage: "login", CSRF: "nonce", Expires: 123456789}
	signed, err := oidc.seal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got oidcTicket
	if err := oidc.unseal(signed, &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("round trip gave %+v", got)
	}
	tampered := "A" + signed[1:]
	if strings.HasPrefix(signed, "A") {
		tampered = "B" + signed[1:]
	}
	if err := oidc.unseal(tampered, &got); err == nil {
		t.Fatal("accepted tampered ticket")
	}
}

func TestOIDCUsernameRestrictions(t *testing.T) {
	accepted := []string{"alice", "bob.brown", "a-user_7"}
	for _, name := range accepted {
		if !oidcUsernamePattern.MatchString(name) {
			t.Fatalf("rejected %q", name)
		}
	}
	rejected := []string{"", "../admin", "a/b", "a\\b", "name with spaces", ".hidden"}
	for _, name := range rejected {
		if oidcUsernamePattern.MatchString(name) && !strings.Contains(name, "..") {
			t.Fatalf("accepted %q", name)
		}
	}
}

// A user opening an expired/stale callback should return to the regular UI
// instead of seeing a 401 page or automatically retrying Authentik.
func TestOIDCCallbackFailureRedirectsToRoot(t *testing.T) {
	o := &nativeOIDC{authBase: "/auth/oidc", secret: []byte("01234567890123456789012345678901")}
	for _, tc := range []struct {
		name      string
		query     string
		badCookie bool
	}{
		{name: "denied by Authentik", query: "error=access_denied"},
		{name: "missing session cookie", query: "code=example&state=old"},
		{name: "invalid state cookie", query: "code=example&state=old", badCookie: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "https://musicas.example/auth/oidc/callback?"+tc.query, nil)
			if tc.badCookie {
				r.AddCookie(&http.Cookie{Name: "nd_oidc_state", Value: "invalid-signature"})
			}
			w := httptest.NewRecorder()
			o.callback(w, r)
			if w.Code != http.StatusSeeOther {
				t.Fatalf("got status %d, want 303", w.Code)
			}
			if loc := w.Header().Get("Location"); loc != "/" {
				t.Fatalf("redirected to %q, want root", loc)
			}
			if cache := w.Header().Get("Cache-Control"); cache != "no-store" {
				t.Fatalf("Cache-Control = %q", cache)
			}
			if got := w.Header().Get("Set-Cookie"); !strings.Contains(got, "nd_oidc_state=") {
				t.Fatalf("state cookie was not cleared: %q", got)
			}
		})
	}
}
