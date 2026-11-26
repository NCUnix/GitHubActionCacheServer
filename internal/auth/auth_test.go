package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NCUnix/GitHubActionCacheServer/internal/testutil"
	"github.com/golang-jwt/jwt/v5"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

const testIssuer = "https://token.actions.githubusercontent.com"

func newJWKSVerifier(t *testing.T, jwks *testutil.JWKS, audience ...string) *Verifier {
	t.Helper()

	verifier, err := NewVerifier(t.Context(), Options{
		Issuer:   testIssuer,
		JWKSURL:  jwks.Server.URL,
		Audience: audience,
	})
	require.NoError(t, err)
	return verifier
}

func validClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"iss":           testIssuer,
		"ac":            `[{"Scope":"refs/heads/main","Permission":3}]`,
		"repository_id": "123",
		"exp":           time.Now().Add(time.Hour).Unix(),
	}
}

func bearer(token string) string {
	return "Bearer " + token
}

// newIssuer serves the OIDC discovery document and, when fallbackJWKS is set,
// the {issuer}/.well-known/jwks fallback; it counts discovery requests.
func newIssuer(t *testing.T, discovery http.HandlerFunc, fallbackJWKS http.Handler) (*httptest.Server, *atomic.Int32) {
	t.Helper()

	var discoveryRequests atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		discoveryRequests.Add(1)
		discovery(w, r)
	})
	if fallbackJWKS != nil {
		mux.Handle("/.well-known/jwks", fallbackJWKS)
	}
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, &discoveryRequests
}

func discoveryDocument(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func requireVerifiesIssuerToken(t *testing.T, verifier *Verifier, jwks *testutil.JWKS, issuer string) {
	t.Helper()

	claims := validClaims()
	claims["iss"] = issuer
	scope, err := verifier.CacheScope(t.Context(), bearer(jwks.Sign(t, jwks.KID, claims)))
	require.NoError(t, err)
	require.Equal(t, "123", scope.RepoID)
}

func TestNewVerifierDiscoversJWKSURL(t *testing.T) {
	jwks := testutil.NewJWKS(t)
	issuer, discoveryRequests := newIssuer(t, discoveryDocument(fmt.Sprintf(`{"jwks_uri":%q}`, jwks.Server.URL)), nil)

	verifier, err := NewVerifier(t.Context(), Options{Issuer: issuer.URL})
	require.NoError(t, err)
	require.Equal(t, int32(1), discoveryRequests.Load())
	requireVerifiesIssuerToken(t, verifier, jwks, issuer.URL)
}

func TestNewVerifierFallsBackWhenDiscoveryFails(t *testing.T) {
	tests := []struct {
		name      string
		discovery http.HandlerFunc
	}{
		{name: "not found", discovery: http.NotFound},
		{name: "missing jwks_uri", discovery: discoveryDocument(`{"issuer":"https://issuer.example"}`)},
		{name: "invalid JSON", discovery: discoveryDocument(`{`)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jwks := testutil.NewJWKS(t)
			issuer, discoveryRequests := newIssuer(t, tt.discovery, jwks.Handler)
			var logs bytes.Buffer
			logger := zerolog.New(&logs)

			verifier, err := NewVerifier(t.Context(), Options{Issuer: issuer.URL, Logger: &logger})
			require.NoError(t, err)
			require.Equal(t, int32(1), discoveryRequests.Load())
			require.Contains(t, logs.String(), `"level":"warn"`)
			require.Contains(t, logs.String(), issuer.URL+"/.well-known/jwks")
			require.Contains(t, logs.String(), "ACTIONS_TOKEN_JWKS_URL")
			requireVerifiesIssuerToken(t, verifier, jwks, issuer.URL)
		})
	}
}

func TestNewVerifierExplicitJWKSURLSkipsDiscovery(t *testing.T) {
	jwks := testutil.NewJWKS(t)
	issuer, discoveryRequests := newIssuer(t, http.NotFound, nil)

	verifier, err := NewVerifier(t.Context(), Options{Issuer: issuer.URL, JWKSURL: jwks.Server.URL})
	require.NoError(t, err)
	require.Zero(t, discoveryRequests.Load())
	requireVerifiesIssuerToken(t, verifier, jwks, issuer.URL)
}

func TestNewVerifierRejectsUnavailableFallbackJWKS(t *testing.T) {
	issuer, _ := newIssuer(t, http.NotFound, nil)

	_, err := NewVerifier(t.Context(), Options{Issuer: issuer.URL})
	require.ErrorContains(t, err, issuer.URL+"/.well-known/jwks")
}

func TestCacheScopeAcceptsValidToken(t *testing.T) {
	jwks := testutil.NewJWKS(t)
	verifier := newJWKSVerifier(t, jwks)

	scope, err := verifier.CacheScope(context.Background(), bearer(jwks.Sign(t, jwks.KID, validClaims())))
	require.NoError(t, err)
	require.Equal(t, "123", scope.RepoID)
}

func TestCacheScopeUnknownKIDIsUnavailableEvenWhenRefreshRateLimited(t *testing.T) {
	jwks := testutil.NewJWKS(t)
	verifier := newJWKSVerifier(t, jwks)

	// First unknown KID consumes the unknown-KID refresh budget.
	_, err := verifier.CacheScope(context.Background(), bearer(jwks.Sign(t, "unknown-1", validClaims())))
	require.ErrorIs(t, err, ErrKeyUnavailable)

	// The rate limiter rejects the second refresh outright; it must still be
	// a 503-class server failure, never a 401.
	_, err = verifier.CacheScope(context.Background(), bearer(jwks.Sign(t, "unknown-2", validClaims())))
	require.ErrorIs(t, err, ErrKeyUnavailable)
}

func TestCacheScopeNonStringKIDIsUnavailable(t *testing.T) {
	jwks := testutil.NewJWKS(t)
	verifier := newJWKSVerifier(t, jwks)

	// Intentional: key resolution faults map to 503 even for malformed kids.
	_, err := verifier.CacheScope(context.Background(), bearer(jwks.Sign(t, 123, validClaims())))
	require.ErrorIs(t, err, ErrKeyUnavailable)
}

func TestCacheScopeRejectsNonRS256Token(t *testing.T) {
	jwks := testutil.NewJWKS(t)
	verifier := newJWKSVerifier(t, jwks)

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, validClaims())
	token.Header["kid"] = jwks.KID
	signed, err := token.SignedString([]byte("attacker-controlled"))
	require.NoError(t, err)

	_, err = verifier.CacheScope(context.Background(), bearer(signed))
	require.ErrorIs(t, err, ErrInvalidToken)
}

func TestCacheScopeRejectsBadSignature(t *testing.T) {
	jwks := testutil.NewJWKS(t)
	verifier := newJWKSVerifier(t, jwks)

	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, validClaims())
	token.Header["kid"] = jwks.KID
	signed, err := token.SignedString(otherKey)
	require.NoError(t, err)

	_, err = verifier.CacheScope(context.Background(), bearer(signed))
	require.ErrorIs(t, err, ErrInvalidToken)
}

func TestCacheScopeRejectsExpiredToken(t *testing.T) {
	jwks := testutil.NewJWKS(t)
	verifier := newJWKSVerifier(t, jwks)

	claims := validClaims()
	claims["exp"] = time.Now().Add(-time.Hour).Unix()
	_, err := verifier.CacheScope(context.Background(), bearer(jwks.Sign(t, jwks.KID, claims)))
	require.ErrorIs(t, err, ErrInvalidToken)
}

func TestCacheScopeRejectsTokenWithoutExpiration(t *testing.T) {
	jwks := testutil.NewJWKS(t)
	verifier := newJWKSVerifier(t, jwks)

	claims := validClaims()
	delete(claims, "exp")
	_, err := verifier.CacheScope(context.Background(), bearer(jwks.Sign(t, jwks.KID, claims)))
	require.ErrorIs(t, err, ErrInvalidToken)
}

func TestCacheScopeAcceptsJustExpiredTokenWithinLeeway(t *testing.T) {
	jwks := testutil.NewJWKS(t)
	verifier := newJWKSVerifier(t, jwks)

	claims := validClaims()
	claims["exp"] = time.Now().Add(-10 * time.Second).Unix()
	scope, err := verifier.CacheScope(context.Background(), bearer(jwks.Sign(t, jwks.KID, claims)))
	require.NoError(t, err)
	require.Equal(t, "123", scope.RepoID)
}

func TestCacheScopeRejectsTokenExpiredBeyondLeeway(t *testing.T) {
	jwks := testutil.NewJWKS(t)
	verifier := newJWKSVerifier(t, jwks)

	claims := validClaims()
	claims["exp"] = time.Now().Add(-40 * time.Second).Unix()
	_, err := verifier.CacheScope(context.Background(), bearer(jwks.Sign(t, jwks.KID, claims)))
	require.ErrorIs(t, err, ErrInvalidToken)
}

func TestCacheScopeRejectsWrongIssuer(t *testing.T) {
	jwks := testutil.NewJWKS(t)
	verifier := newJWKSVerifier(t, jwks)

	claims := validClaims()
	claims["iss"] = "https://evil.example"
	_, err := verifier.CacheScope(context.Background(), bearer(jwks.Sign(t, jwks.KID, claims)))
	require.ErrorIs(t, err, ErrInvalidToken)
}

func TestCacheScopeAudience(t *testing.T) {
	tests := []struct {
		name       string
		audience   []string
		tokenAud   any
		wantJWTErr error
	}{
		{name: "string aud matches", audience: []string{"first", "second"}, tokenAud: "second"},
		{name: "array aud matches", audience: []string{"first", "second"}, tokenAud: []string{"other", "first"}},
		{name: "aud does not match", audience: []string{"first", "second"}, tokenAud: []string{"other"}, wantJWTErr: jwt.ErrTokenInvalidAudience},
		{name: "aud missing", audience: []string{"first"}, wantJWTErr: jwt.ErrTokenRequiredClaimMissing},
		{name: "audience not configured", tokenAud: "anything"},
	}

	jwks := testutil.NewJWKS(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			verifier := newJWKSVerifier(t, jwks, tt.audience...)
			claims := validClaims()
			if tt.tokenAud != nil {
				claims["aud"] = tt.tokenAud
			}

			scope, err := verifier.CacheScope(t.Context(), bearer(jwks.Sign(t, jwks.KID, claims)))
			if tt.wantJWTErr != nil {
				require.ErrorIs(t, err, ErrInvalidToken)
				require.ErrorIs(t, err, tt.wantJWTErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "123", scope.RepoID)
		})
	}
}

func TestNewVerifierRejectsMalformedJWKSURL(t *testing.T) {
	_, err := NewVerifier(context.Background(), Options{
		Issuer:  testIssuer,
		JWKSURL: "://invalid",
	})
	require.Error(t, err)
}

func TestNewVerifierRejectsUnreachableJWKS(t *testing.T) {
	server := httptest.NewServer(nil)
	server.Close()

	_, err := NewVerifier(t.Context(), Options{
		Issuer:  testIssuer,
		JWKSURL: server.URL,
	})
	require.Error(t, err)
}

func TestCacheScopeSkipValidationDecodesWithoutVerification(t *testing.T) {
	verifier, err := NewVerifier(context.Background(), Options{SkipValidation: true})
	require.NoError(t, err)

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"ac":            `[{"Scope":"refs/heads/main","Permission":3}]`,
		"repository_id": "123",
	})
	signed, err := token.SignedString([]byte("mock-secret-key"))
	require.NoError(t, err)

	scope, err := verifier.CacheScope(context.Background(), bearer(signed))
	require.NoError(t, err)
	require.Equal(t, "123", scope.RepoID)
}

func TestCacheScopeSkipValidationIgnoresAudience(t *testing.T) {
	verifier, err := NewVerifier(t.Context(), Options{SkipValidation: true, Audience: []string{"expected"}})
	require.NoError(t, err)

	claims := validClaims()
	claims["aud"] = "other"
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("mock-secret-key"))
	require.NoError(t, err)

	scope, err := verifier.CacheScope(t.Context(), bearer(signed))
	require.NoError(t, err)
	require.Equal(t, "123", scope.RepoID)
}
