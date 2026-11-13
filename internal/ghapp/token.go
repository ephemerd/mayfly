package ghapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// InstallationToken mints a short-lived installation access token from the
// App credentials: sign a JWT with the PEM, exchange it at
// /app/installations/{id}/access_tokens. Tokens last ~1h; callers use them
// immediately and discard.
func (c *Credentials) InstallationToken(ctx context.Context) (string, error) {
	jwt, err := c.appJWT()
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", c.InstallationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("mint installation token: %s", resp.Status)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", fmt.Errorf("mint installation token: empty token in response")
	}
	return out.Token, nil
}

// appJWT builds the RS256 App JWT (iss = app id, 9-minute window).
func (c *Credentials) appJWT() (string, error) {
	key, err := parseRSAKey(c.PrivateKeyPEM)
	if err != nil {
		return "", err
	}
	now := time.Now().Add(-30 * time.Second)
	header := b64url(`{"alg":"RS256","typ":"JWT"}`)
	claims := b64url(fmt.Sprintf(`{"iat":%d,"exp":%d,"iss":"%s"}`,
		now.Unix(), now.Add(9*time.Minute).Unix(), strconv.FormatInt(c.AppID, 10)))
	signingInput := header + "." + claims

	h := sha256.Sum256([]byte(signingInput))
	sig, err := key.Sign(rand.Reader, h[:], crypto.SHA256)
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func parseRSAKey(b []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, fmt.Errorf("private key is not PEM-encoded")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	signer, ok := k.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("private key is not a signer")
	}
	return signer, nil
}

func b64url(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strings.TrimSpace(s)))
}
