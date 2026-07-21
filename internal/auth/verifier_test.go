package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestVerifier(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": "test-key", "kty": "RSA", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
		}}})
	}))
	defer server.Close()

	verifier, err := NewVerifier(Config{Issuer: "https://issuer.test", JWKSURL: server.URL, Audience: "noapp-api"})
	if err != nil {
		t.Fatal(err)
	}
	valid := signTestToken(t, key, map[string]any{
		"iss": "https://issuer.test", "sub": "subject-1", "aud": []string{"account", "noapp-api"},
		"exp": time.Now().Add(time.Minute).Unix(), "preferred_username": "editor",
		"realm_access": map[string]any{"roles": []string{"noapp-editor", "noapp-viewer"}},
	})
	principal, err := verifier.Verify(context.Background(), "Bearer "+valid)
	if err != nil {
		t.Fatalf("verify valid token: %v", err)
	}
	if principal.Subject != "subject-1" || principal.Username != "editor" || !principal.HasRole("noapp-editor") {
		t.Fatalf("unexpected principal: %#v", principal)
	}

	for name, authorization := range map[string]string{
		"missing":  "",
		"tampered": "Bearer " + valid[:len(valid)-1] + "x",
		"expired": "Bearer " + signTestToken(t, key, map[string]any{
			"iss": "https://issuer.test", "sub": "subject-1", "aud": "noapp-api", "exp": time.Now().Add(-time.Minute).Unix(),
		}),
		"wrong audience": "Bearer " + signTestToken(t, key, map[string]any{
			"iss": "https://issuer.test", "sub": "subject-1", "aud": "another-api", "exp": time.Now().Add(time.Minute).Unix(),
		}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := verifier.Verify(context.Background(), authorization); err == nil {
				t.Fatal("expected verification failure")
			}
		})
	}
}

func signTestToken(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "test-key", "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	unsigned := fmt.Sprintf("%s.%s", base64.RawURLEncoding.EncodeToString(header), base64.RawURLEncoding.EncodeToString(payload))
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
}
