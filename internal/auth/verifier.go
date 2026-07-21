package auth

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

var (
	ErrMissingToken = errors.New("bearer token is required")
	ErrInvalidToken = errors.New("bearer token is invalid")
)

type Config struct {
	Issuer   string
	JWKSURL  string
	Audience string
}

type Principal struct {
	Subject   string
	Username  string
	Roles     []string
	ExpiresAt time.Time
}

func (p Principal) HasRole(role string) bool {
	for _, candidate := range p.Roles {
		if candidate == role {
			return true
		}
	}
	return false
}

type Verifier struct {
	config  Config
	http    *http.Client
	mu      sync.RWMutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

func NewVerifier(config Config) (*Verifier, error) {
	if config.Issuer == "" || config.JWKSURL == "" || config.Audience == "" {
		return nil, errors.New("issuer, JWKS URL, and audience are required")
	}
	return &Verifier{
		config: config,
		http:   &http.Client{Timeout: 5 * time.Second},
		keys:   make(map[string]*rsa.PublicKey),
	}, nil
}

type accessClaims struct {
	Issuer            string          `json:"iss"`
	Subject           string          `json:"sub"`
	Audience          json.RawMessage `json:"aud"`
	ExpiresAt         int64           `json:"exp"`
	NotBefore         int64           `json:"nbf"`
	PreferredUsername string          `json:"preferred_username"`
	RealmAccess       struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
}

func (v *Verifier) Verify(ctx context.Context, authorization string) (Principal, error) {
	const prefix = "Bearer "
	if !strings.HasPrefix(authorization, prefix) || strings.TrimSpace(strings.TrimPrefix(authorization, prefix)) == "" {
		return Principal{}, ErrMissingToken
	}

	encodedToken := strings.TrimSpace(strings.TrimPrefix(authorization, prefix))
	parts := strings.Split(encodedToken, ".")
	if len(parts) != 3 {
		return Principal{}, ErrInvalidToken
	}
	var header struct {
		Algorithm string `json:"alg"`
		KID       string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &header); err != nil || header.Algorithm != "RS256" || header.KID == "" {
		return Principal{}, ErrInvalidToken
	}
	claims := &accessClaims{}
	if err := decodeSegment(parts[1], claims); err != nil {
		return Principal{}, ErrInvalidToken
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Principal{}, ErrInvalidToken
	}
	key, err := v.key(ctx, header.KID)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature); err != nil {
		return Principal{}, ErrInvalidToken
	}
	now := time.Now().Unix()
	if claims.Issuer != v.config.Issuer || claims.Subject == "" || claims.ExpiresAt <= now ||
		(claims.NotBefore != 0 && claims.NotBefore > now) || !hasAudience(claims.Audience, v.config.Audience) {
		return Principal{}, ErrInvalidToken
	}
	return Principal{
		Subject:   claims.Subject,
		Username:  claims.PreferredUsername,
		Roles:     append([]string(nil), claims.RealmAccess.Roles...),
		ExpiresAt: time.Unix(claims.ExpiresAt, 0),
	}, nil
}

func decodeSegment(segment string, value any) error {
	payload, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return err
	}
	return json.Unmarshal(payload, value)
}

func hasAudience(raw json.RawMessage, expected string) bool {
	var single string
	if json.Unmarshal(raw, &single) == nil {
		return single == expected
	}
	var multiple []string
	if json.Unmarshal(raw, &multiple) != nil {
		return false
	}
	for _, audience := range multiple {
		if audience == expected {
			return true
		}
	}
	return false
}

func (v *Verifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.RLock()
	key := v.keys[kid]
	fresh := time.Since(v.fetched) < 5*time.Minute
	v.mu.RUnlock()
	if key != nil && fresh {
		return key, nil
	}
	if err := v.refresh(ctx); err != nil {
		return nil, err
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	key = v.keys[kid]
	if key == nil {
		return nil, fmt.Errorf("no signing key for kid %q", kid)
	}
	return key, nil
}

func (v *Verifier) refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.config.JWKSURL, nil)
	if err != nil {
		return err
	}
	response, err := v.http.Do(req)
	if err != nil {
		return fmt.Errorf("fetch JWKS: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch JWKS: unexpected status %d", response.StatusCode)
	}
	var document struct {
		Keys []struct {
			KID string `json:"kid"`
			KTY string `json:"kty"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		return fmt.Errorf("decode JWKS: %w", err)
	}
	keys := make(map[string]*rsa.PublicKey)
	for _, jwk := range document.Keys {
		if jwk.KID == "" || jwk.KTY != "RSA" || (jwk.Use != "" && jwk.Use != "sig") {
			continue
		}
		modulus, err := base64.RawURLEncoding.DecodeString(jwk.N)
		if err != nil {
			continue
		}
		exponent, err := base64.RawURLEncoding.DecodeString(jwk.E)
		if err != nil || len(exponent) == 0 {
			continue
		}
		keys[jwk.KID] = &rsa.PublicKey{N: new(big.Int).SetBytes(modulus), E: int(new(big.Int).SetBytes(exponent).Int64())}
	}
	if len(keys) == 0 {
		return errors.New("JWKS contains no RSA signing keys")
	}
	v.mu.Lock()
	v.keys = keys
	v.fetched = time.Now()
	v.mu.Unlock()
	return nil
}
