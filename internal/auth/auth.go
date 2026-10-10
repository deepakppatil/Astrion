// Package auth authenticates API callers (AUTH-001). Production deployments
// use OIDC bearer tokens (JWT access/ID tokens) verified against the
// identity provider's JWKS. A static-token development authenticator exists
// for local work and is refused when ASTRION_ENV=production.
//
// Authentication yields an Identity; mapping an Identity to a tenant user
// and roles is done by the API layer so that role assignments are always
// resolved and enforced server-side.
package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/deepakppatil/astrion/internal/domain"
)

var (
	ErrNoCredentials = errors.New("missing bearer token")
	ErrInvalidToken  = errors.New("invalid token")
)

// Identity is an authenticated external identity.
type Identity struct {
	Subject  string
	Email    string
	Name     string
	TenantID string        // resolved tenant (claim or default)
	Groups   []string      // IdP groups, mapped to roles by RoleMapper
	Roles    []domain.Role // explicit roles (dev authenticator only)
	// RolesAuthoritative is true when Roles/Groups should replace stored
	// role assignments on login (IdP-managed roles).
	RolesAuthoritative bool
}

type Authenticator interface {
	Authenticate(r *http.Request) (*Identity, error)
}

func BearerToken(r *http.Request) (string, error) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", ErrNoCredentials
	}
	scheme, tok, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(tok) == "" {
		return "", ErrNoCredentials
	}
	return strings.TrimSpace(tok), nil
}

// ---------------------------------------------------------------------------
// Development static tokens

// DevUser is a static development identity.
type DevUser struct {
	Token   string
	Subject string
	Name    string
	Email   string
	Roles   []domain.Role
}

type Dev struct {
	TenantID string
	users    []DevUser
}

// NewDev returns a development authenticator. It refuses to run when
// production is true (AUTH-001: no shared or static credentials in prod).
func NewDev(production bool, tenantID string, users []DevUser) (*Dev, error) {
	if production {
		return nil, errors.New("static development tokens are not permitted when ASTRION_ENV=production")
	}
	for _, u := range users {
		if len(u.Token) < 8 {
			return nil, fmt.Errorf("dev token for %s is too short", u.Subject)
		}
	}
	return &Dev{TenantID: tenantID, users: users}, nil
}

// ParseDevUsers parses "token:subject:role1|role2;token2:subject2:role".
func ParseDevUsers(spec string) ([]DevUser, error) {
	var out []DevUser
	for _, entry := range strings.Split(spec, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, ":")
		if len(parts) != 3 {
			return nil, fmt.Errorf("invalid dev user entry %q (want token:subject:roles)", entry)
		}
		u := DevUser{Token: parts[0], Subject: parts[1], Name: parts[1], Email: parts[1] + "@dev.local"}
		for _, r := range strings.Split(parts[2], "|") {
			role := domain.Role(strings.TrimSpace(r))
			if !role.Valid() {
				return nil, fmt.Errorf("unknown role %q for %s", r, parts[1])
			}
			u.Roles = append(u.Roles, role)
		}
		out = append(out, u)
	}
	return out, nil
}

func (d *Dev) Authenticate(r *http.Request) (*Identity, error) {
	tok, err := BearerToken(r)
	if err != nil {
		return nil, err
	}
	for _, u := range d.users {
		if subtle.ConstantTimeCompare([]byte(tok), []byte(u.Token)) == 1 {
			return &Identity{Subject: u.Subject, Name: u.Name, Email: u.Email, TenantID: d.TenantID,
				Roles: append([]domain.Role(nil), u.Roles...), RolesAuthoritative: true}, nil
		}
	}
	return nil, ErrInvalidToken
}

// ---------------------------------------------------------------------------
// OIDC

type OIDCConfig struct {
	Issuer   string
	Audience string
	// JWKSURL overrides discovery.
	JWKSURL string
	// TenantClaim names a claim carrying the tenant ID. If empty, every
	// identity belongs to DefaultTenant (single-tenant deployment).
	TenantClaim   string
	DefaultTenant string
	GroupsClaim   string // default "groups"
	// GroupRoles maps IdP groups to Astrion roles. If non-empty, IdP groups
	// are authoritative for role assignment.
	GroupRoles map[string][]domain.Role
	Leeway     time.Duration
	HTTPClient *http.Client
}

type OIDC struct {
	cfg OIDCConfig
	now func() time.Time

	mu          sync.Mutex
	keys        map[string]crypto.PublicKey
	jwksURL     string
	lastRefresh time.Time
}

func NewOIDC(cfg OIDCConfig) (*OIDC, error) {
	if cfg.Issuer == "" || cfg.Audience == "" {
		return nil, errors.New("oidc issuer and audience are required")
	}
	if cfg.GroupsClaim == "" {
		cfg.GroupsClaim = "groups"
	}
	if cfg.Leeway == 0 {
		cfg.Leeway = 60 * time.Second
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.TenantClaim == "" && cfg.DefaultTenant == "" {
		return nil, errors.New("either a tenant claim or a default tenant is required")
	}
	return &OIDC{cfg: cfg, now: time.Now, keys: map[string]crypto.PublicKey{}, jwksURL: cfg.JWKSURL}, nil
}

func (o *OIDC) Authenticate(r *http.Request) (*Identity, error) {
	tok, err := BearerToken(r)
	if err != nil {
		return nil, err
	}
	claims, err := o.Verify(r.Context(), tok)
	if err != nil {
		return nil, err
	}
	id := &Identity{}
	id.Subject, _ = claims["sub"].(string)
	id.Email, _ = claims["email"].(string)
	id.Name, _ = claims["name"].(string)
	if id.Name == "" {
		id.Name, _ = claims["preferred_username"].(string)
	}
	if id.Subject == "" {
		return nil, fmt.Errorf("%w: missing sub", ErrInvalidToken)
	}
	if o.cfg.TenantClaim != "" {
		id.TenantID, _ = claims[o.cfg.TenantClaim].(string)
		if id.TenantID == "" {
			return nil, fmt.Errorf("%w: missing tenant claim", ErrInvalidToken)
		}
	} else {
		id.TenantID = o.cfg.DefaultTenant
	}
	if gs, ok := claims[o.cfg.GroupsClaim].([]any); ok {
		for _, g := range gs {
			if s, ok := g.(string); ok {
				id.Groups = append(id.Groups, s)
			}
		}
	}
	if len(o.cfg.GroupRoles) > 0 {
		id.RolesAuthoritative = true
		seen := map[domain.Role]bool{}
		for _, g := range id.Groups {
			for _, role := range o.cfg.GroupRoles[g] {
				if !seen[role] {
					seen[role] = true
					id.Roles = append(id.Roles, role)
				}
			}
		}
	}
	return id, nil
}

// Verify checks signature, issuer, audience and time claims.
func (o *OIDC) Verify(ctx context.Context, token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, ErrInvalidToken
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
		Typ string `json:"typ"`
	}
	if err := decodeSegment(parts[0], &header); err != nil {
		return nil, ErrInvalidToken
	}
	key, err := o.key(ctx, header.Kid)
	if err != nil {
		return nil, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, ErrInvalidToken
	}
	if err := verifySignature(header.Alg, key, []byte(parts[0]+"."+parts[1]), sig); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	var claims map[string]any
	if err := decodeSegment(parts[1], &claims); err != nil {
		return nil, ErrInvalidToken
	}
	if iss, _ := claims["iss"].(string); iss != o.cfg.Issuer {
		return nil, fmt.Errorf("%w: issuer mismatch", ErrInvalidToken)
	}
	if !audienceMatches(claims["aud"], o.cfg.Audience) {
		return nil, fmt.Errorf("%w: audience mismatch", ErrInvalidToken)
	}
	now := o.now()
	exp, ok := numericDate(claims["exp"])
	if !ok || now.After(exp.Add(o.cfg.Leeway)) {
		return nil, fmt.Errorf("%w: expired", ErrInvalidToken)
	}
	if nbf, ok := numericDate(claims["nbf"]); ok && now.Add(o.cfg.Leeway).Before(nbf) {
		return nil, fmt.Errorf("%w: not yet valid", ErrInvalidToken)
	}
	return claims, nil
}

func (o *OIDC) key(ctx context.Context, kid string) (crypto.PublicKey, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if k, ok := o.keys[kid]; ok {
		return k, nil
	}
	// Unknown kid: refresh JWKS (handles key rotation), rate limited.
	if time.Since(o.lastRefresh) < 30*time.Second && len(o.keys) > 0 {
		return nil, fmt.Errorf("%w: unknown key id", ErrInvalidToken)
	}
	if err := o.refresh(ctx); err != nil {
		return nil, fmt.Errorf("jwks refresh: %w", err)
	}
	if k, ok := o.keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("%w: unknown key id", ErrInvalidToken)
}

func (o *OIDC) refresh(ctx context.Context) error {
	o.lastRefresh = time.Now()
	if o.jwksURL == "" {
		var disc struct {
			Issuer  string `json:"issuer"`
			JWKSURI string `json:"jwks_uri"`
		}
		if err := o.getJSON(ctx, strings.TrimRight(o.cfg.Issuer, "/")+"/.well-known/openid-configuration", &disc); err != nil {
			return err
		}
		if disc.Issuer != o.cfg.Issuer || disc.JWKSURI == "" {
			return errors.New("discovery document issuer mismatch or missing jwks_uri")
		}
		o.jwksURL = disc.JWKSURI
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := o.getJSON(ctx, o.jwksURL, &set); err != nil {
		return err
	}
	keys := map[string]crypto.PublicKey{}
	for _, k := range set.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		pk, err := k.publicKey()
		if err == nil {
			keys[k.Kid] = pk
		}
	}
	if len(keys) == 0 {
		return errors.New("jwks contains no usable signing keys")
	}
	o.keys = keys
	return nil
}

func (o *OIDC) getJSON(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := o.cfg.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("GET %s: %d", url, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func (k jwk) publicKey() (crypto.PublicKey, error) {
	b := func(s string) (*big.Int, error) {
		raw, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			return nil, err
		}
		return new(big.Int).SetBytes(raw), nil
	}
	switch k.Kty {
	case "RSA":
		n, err := b(k.N)
		if err != nil {
			return nil, err
		}
		e, err := b(k.E)
		if err != nil {
			return nil, err
		}
		if n.BitLen() < 2048 {
			return nil, errors.New("rsa key too small")
		}
		return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
	case "EC":
		var curve elliptic.Curve
		switch k.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		default:
			return nil, fmt.Errorf("unsupported curve %s", k.Crv)
		}
		x, err := b(k.X)
		if err != nil {
			return nil, err
		}
		y, err := b(k.Y)
		if err != nil {
			return nil, err
		}
		return &ecdsa.PublicKey{Curve: curve, X: x, Y: y}, nil
	}
	return nil, fmt.Errorf("unsupported key type %s", k.Kty)
}

// verifySignature supports RS256/384/512 and ES256/384. "none" and HMAC
// algorithms are rejected (prevents algorithm-confusion attacks).
func verifySignature(alg string, key crypto.PublicKey, signed, sig []byte) error {
	var h hash.Hash
	var ch crypto.Hash
	switch alg {
	case "RS256", "ES256":
		h, ch = sha256.New(), crypto.SHA256
	case "RS384", "ES384":
		h, ch = sha512.New384(), crypto.SHA384
	case "RS512":
		h, ch = sha512.New(), crypto.SHA512
	default:
		return fmt.Errorf("unsupported alg %q", alg)
	}
	h.Write(signed)
	digest := h.Sum(nil)
	switch k := key.(type) {
	case *rsa.PublicKey:
		if !strings.HasPrefix(alg, "RS") {
			return errors.New("alg does not match key type")
		}
		return rsa.VerifyPKCS1v15(k, ch, digest, sig)
	case *ecdsa.PublicKey:
		if !strings.HasPrefix(alg, "ES") {
			return errors.New("alg does not match key type")
		}
		size := (k.Curve.Params().BitSize + 7) / 8
		if len(sig) != 2*size {
			return errors.New("bad ecdsa signature length")
		}
		r, s := new(big.Int).SetBytes(sig[:size]), new(big.Int).SetBytes(sig[size:])
		if !ecdsa.Verify(k, digest, r, s) {
			return errors.New("ecdsa verification failed")
		}
		return nil
	}
	return errors.New("unsupported key")
}

func decodeSegment(seg string, v any) error {
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

func audienceMatches(aud any, want string) bool {
	switch a := aud.(type) {
	case string:
		return a == want
	case []any:
		for _, x := range a {
			if s, ok := x.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

func numericDate(v any) (time.Time, bool) {
	f, ok := v.(float64)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(int64(f), 0), true
}
