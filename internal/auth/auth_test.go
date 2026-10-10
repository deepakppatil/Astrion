package auth

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/deepakppatil/astrion/internal/domain"
)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func sign(t *testing.T, key *rsa.PrivateKey, kid, alg string, claims map[string]any) string {
	h, _ := json.Marshal(map[string]string{"alg": alg, "kid": kid, "typ": "JWT"})
	c, _ := json.Marshal(claims)
	signed := b64(h) + "." + b64(c)
	d := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, d[:])
	if err != nil {
		t.Fatal(err)
	}
	return signed + "." + b64(sig)
}

func idp(t *testing.T, key *rsa.PrivateKey) *httptest.Server {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]string{"issuer": srv.URL, "jwks_uri": srv.URL + "/jwks"})
		case "/jwks":
			json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
				"kty": "RSA", "kid": "k1", "use": "sig",
				"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
			}}})
		default:
			w.WriteHeader(404)
		}
	}))
	return srv
}

func req(tok string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	return r
}

func TestOIDC(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := idp(t, key)
	defer srv.Close()
	o, err := NewOIDC(OIDCConfig{Issuer: srv.URL, Audience: "astrion", TenantClaim: "tid",
		GroupRoles: map[string][]domain.Role{"eng": {domain.RoleDeveloper}, "arch": {domain.RoleArchitect}}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	good := map[string]any{"iss": srv.URL, "aud": []any{"astrion", "other"}, "sub": "u1", "email": "u1@x",
		"tid": "ten_1", "groups": []any{"eng", "unknown"}, "exp": now + 300, "iat": now}

	id, err := o.Authenticate(req(sign(t, key, "k1", "RS256", good)))
	if err != nil {
		t.Fatal(err)
	}
	if id.Subject != "u1" || id.TenantID != "ten_1" || len(id.Roles) != 1 || id.Roles[0] != domain.RoleDeveloper || !id.RolesAuthoritative {
		t.Fatalf("unexpected identity %+v", id)
	}

	bad := func(name string, mutate func(map[string]any), tok func(map[string]any) string) {
		c := map[string]any{}
		for k, v := range good {
			c[k] = v
		}
		if mutate != nil {
			mutate(c)
		}
		if _, err := o.Authenticate(req(tok(c))); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
	std := func(c map[string]any) string { return sign(t, key, "k1", "RS256", c) }
	bad("expired", func(c map[string]any) { c["exp"] = now - 3600 }, std)
	bad("issuer", func(c map[string]any) { c["iss"] = "https://evil" }, std)
	bad("audience", func(c map[string]any) { c["aud"] = "other" }, std)
	bad("no tenant", func(c map[string]any) { delete(c, "tid") }, std)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	bad("wrong key", nil, func(c map[string]any) string { return sign(t, other, "k1", "RS256", c) })
	bad("alg none", nil, func(c map[string]any) string {
		h, _ := json.Marshal(map[string]string{"alg": "none", "kid": "k1"})
		p, _ := json.Marshal(c)
		return b64(h) + "." + b64(p) + "."
	})
	bad("tampered", nil, func(c map[string]any) string {
		tok := std(c)
		p, _ := json.Marshal(map[string]any{"iss": srv.URL, "aud": "astrion", "sub": "admin", "tid": "ten_1", "exp": now + 300})
		parts := splitDots(tok)
		return parts[0] + "." + b64(p) + "." + parts[2]
	})
	if _, err := o.Authenticate(req("")); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("expected ErrNoCredentials, got %v", err)
	}
}

func splitDots(s string) []string {
	var out []string
	start := 0
	for i := range s {
		if s[i] == '.' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func TestDevAuthenticator(t *testing.T) {
	users, err := ParseDevUsers("dev-alice-token:alice:developer|architect;dev-bob-token:bob:business_analyst")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewDev(true, "ten", users); err == nil {
		t.Fatal("dev authenticator must be refused in production")
	}
	d, err := NewDev(false, "ten_dev", users)
	if err != nil {
		t.Fatal(err)
	}
	id, err := d.Authenticate(req("dev-alice-token"))
	if err != nil || id.Subject != "alice" || len(id.Roles) != 2 || id.TenantID != "ten_dev" {
		t.Fatalf("%+v %v", id, err)
	}
	if _, err := d.Authenticate(req("nope-nope-nope")); err == nil {
		t.Fatal("unknown token accepted")
	}
	if _, err := ParseDevUsers("tok:alice:superuser"); err == nil {
		t.Fatal("unknown role accepted")
	}
}
