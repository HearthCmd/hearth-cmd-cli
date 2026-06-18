package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// serviceAccountKey is the shape of the JSON key file downloaded from
// Google Cloud Console for a service account.
type serviceAccountKey struct {
	Type         string `json:"type"`
	ProjectID    string `json:"project_id"`
	ClientEmail  string `json:"client_email"`
	ClientID     string `json:"client_id"`
	PrivateKey   string `json:"private_key"`
	PrivateKeyID string `json:"private_key_id"`
	TokenURI     string `json:"token_uri"`
}

// gcpCachedToken holds a short-lived access token and its expiry.
type gcpCachedToken struct {
	token  string
	expiry time.Time
}

// gcpTokenCache caches access tokens keyed by (service-account-email +
// impersonated-email + scopes). Multiple connections using the same
// service account share a cached token; a new exchange only fires when
// the cached token is within 30 s of expiry.
type gcpTokenCache struct {
	mu    sync.Mutex
	cache map[string]gcpCachedToken
}

func newGCPTokenCache() *gcpTokenCache {
	return &gcpTokenCache{cache: map[string]gcpCachedToken{}}
}

func (c *gcpTokenCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.cache[key]
	if !ok || time.Now().Add(30*time.Second).After(t.expiry) {
		return "", false
	}
	return t.token, true
}

func (c *gcpTokenCache) set(key, token string, expiry time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache[key] = gcpCachedToken{token: token, expiry: expiry}
}

// oauthAccessCache caches short-lived OAuth access tokens keyed by a
// hash of the refresh token. Avoids redundant server round-trips when
// the same connection identity is used within a token's lifetime.
type oauthAccessCache struct {
	mu    sync.Mutex
	cache map[string]gcpCachedToken // reuse same struct (token + expiry)
}

func newOAuthAccessCache() *oauthAccessCache {
	return &oauthAccessCache{cache: map[string]gcpCachedToken{}}
}

func (c *oauthAccessCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.cache[key]
	if !ok || time.Now().Add(60*time.Second).After(t.expiry) {
		return "", false
	}
	return t.token, true
}

func (c *oauthAccessCache) set(key, token string, expiry time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache[key] = gcpCachedToken{token: token, expiry: expiry}
}

// resolveServiceAccountToken exchanges a service account JSON key for
// a short-lived Google access token. impersonateEmail is the Workspace
// user to act as (domain-wide delegation); pass empty to issue a token
// for the service account itself. Results are cached until 30 s before
// expiry.
func (x *DeclarativeExecutor) resolveServiceAccountToken(
	ctx context.Context,
	jsonKey string,
	impersonateEmail string,
	scopes []string,
) (string, error) {
	var key serviceAccountKey
	if err := json.Unmarshal([]byte(jsonKey), &key); err != nil {
		return "", fmt.Errorf("service_account_json: parse key: %w", err)
	}
	if key.ClientEmail == "" {
		return "", fmt.Errorf("service_account_json: key missing client_email")
	}
	if key.PrivateKey == "" {
		return "", fmt.Errorf("service_account_json: key missing private_key")
	}
	tokenURI := key.TokenURI
	if tokenURI == "" {
		tokenURI = "https://oauth2.googleapis.com/token"
	}

	cacheKey := key.ClientEmail + "|" + impersonateEmail + "|" + strings.Join(scopes, ",")
	if token, ok := x.gcpTokenCache.get(cacheKey); ok {
		return token, nil
	}

	token, expiry, err := exchangeServiceAccountToken(ctx, x.client, key, impersonateEmail, scopes, tokenURI)
	if err != nil {
		return "", err
	}
	x.gcpTokenCache.set(cacheKey, token, expiry)
	return token, nil
}

// exchangeServiceAccountToken builds a signed RS256 JWT and POSTs it
// to the Google OAuth2 token endpoint, returning the access token and
// its expiry.
func exchangeServiceAccountToken(
	ctx context.Context,
	client *http.Client,
	key serviceAccountKey,
	impersonateEmail string,
	scopes []string,
	tokenURI string,
) (string, time.Time, error) {
	now := time.Now()
	exp := now.Add(time.Hour)

	headerJSON := `{"alg":"RS256","typ":"JWT"}`
	header := base64.RawURLEncoding.EncodeToString([]byte(headerJSON))

	claims := map[string]any{
		"iss":   key.ClientEmail,
		"scope": strings.Join(scopes, " "),
		"aud":   tokenURI,
		"exp":   exp.Unix(),
		"iat":   now.Unix(),
	}
	if impersonateEmail != "" {
		claims["sub"] = impersonateEmail
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("service_account: marshal claims: %w", err)
	}
	payload := header + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)

	block, _ := pem.Decode([]byte(key.PrivateKey))
	if block == nil {
		return "", time.Time{}, fmt.Errorf("service_account: decode private key PEM: empty block")
	}
	privKeyAny, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("service_account: parse private key: %w", err)
	}
	rsaKey, ok := privKeyAny.(*rsa.PrivateKey)
	if !ok {
		return "", time.Time{}, fmt.Errorf("service_account: private key is not RSA (got %T)", privKeyAny)
	}

	digest := sha256.Sum256([]byte(payload))
	sig, err := rsa.SignPKCS1v15(rand.Reader, rsaKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", time.Time{}, fmt.Errorf("service_account: sign JWT: %w", err)
	}
	jwt := payload + "." + base64.RawURLEncoding.EncodeToString(sig)

	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {jwt},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("service_account: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("service_account: token exchange: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("service_account: token exchange status %d: %s",
			resp.StatusCode, snippet(body, 256))
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", time.Time{}, fmt.Errorf("service_account: parse token response: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("service_account: token response missing access_token")
	}
	ttl := time.Duration(tokenResp.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}
	return tokenResp.AccessToken, time.Now().Add(ttl), nil
}

// expandCredentials builds the map[string]any credentials scope for
// the declarative template engine. For plain credentials the value is
// the raw secret string, accessible as {{credentials.name}}. For
// service_account_json credentials the value is a sub-map with key
// "access_token", accessible as {{credentials.name.access_token}}.
//
// Call this once per invoke / snapshot, before assembling
// DeclarativeInvokeInput or DeclarativeSnapshotInput.
func (x *DeclarativeExecutor) expandCredentials(
	ctx context.Context,
	manifestCreds []PluginCredential,
	secretMap map[string]string,
	config map[string]any,
) (map[string]any, error) {
	out := make(map[string]any, len(secretMap))
	for k, v := range secretMap {
		out[k] = v
	}
	for _, mc := range manifestCreds {
		switch mc.Type {
		case "service_account_json":
			rawJSON, ok := secretMap[mc.Name]
			if !ok || rawJSON == "" {
				continue
			}
			impersonateEmail := ""
			if mc.ImpersonationConfigRef != "" {
				if v, ok := config[mc.ImpersonationConfigRef]; ok {
					if s, ok := v.(string); ok {
						impersonateEmail = s
					}
				}
			}
			if len(mc.Scopes) == 0 {
				return nil, fmt.Errorf("credential %q: type service_account_json requires at least one scope", mc.Name)
			}
			token, err := x.resolveServiceAccountToken(ctx, rawJSON, impersonateEmail, mc.Scopes)
			if err != nil {
				return nil, fmt.Errorf("credential %q: %w", mc.Name, err)
			}
			out[mc.Name] = map[string]any{"access_token": token}

		case "oauth2_google":
			refreshToken, ok := secretMap[mc.Name]
			if !ok || refreshToken == "" {
				continue
			}
			if x.oauthExchanger == nil {
				return nil, fmt.Errorf("credential %q: oauth2_google requires an OAuthTokenExchanger (daemon not configured)", mc.Name)
			}
			// Cache keyed by SHA-256 of the refresh token so we don't
			// log or store the raw token as a map key.
			cacheKey := fmt.Sprintf("%x", sha256Sum([]byte(refreshToken)))
			if token, ok := x.oauthTokenCache.get(cacheKey); ok {
				out[mc.Name] = map[string]any{"access_token": token}
				continue
			}
			accessToken, expiresIn, err := x.oauthExchanger.ExchangeOAuthToken(ctx, "google", []byte(refreshToken))
			if err != nil {
				return nil, fmt.Errorf("credential %q: exchange oauth token: %w", mc.Name, err)
			}
			ttl := time.Duration(expiresIn) * time.Second
			if ttl <= 0 {
				ttl = time.Hour
			}
			x.oauthTokenCache.set(cacheKey, accessToken, time.Now().Add(ttl))
			out[mc.Name] = map[string]any{"access_token": accessToken}
		}
	}
	return out, nil
}

func sha256Sum(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}
