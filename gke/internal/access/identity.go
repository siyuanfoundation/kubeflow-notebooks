package access

import (
	"context"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

const (
	IAPHeader  = "X-Goog-Iap-Jwt-Assertion"
	iapIssuer  = "https://cloud.google.com/iap"
	iapKeysURL = "https://www.gstatic.com/iap/verify/public_key-jwk"
)

type Identity struct {
	Email   string
	Subject string
}

type Authenticator interface {
	Authenticate(context.Context, string) (Identity, error)
}

type IAPAuthenticator struct {
	verifier *oidc.IDTokenVerifier
	audience string
}

func NewIAPAuthenticator(ctx context.Context, audience string) (*IAPAuthenticator, error) {
	return newIAPAuthenticator(audience, oidc.NewRemoteKeySet(ctx, iapKeysURL))
}

func newIAPAuthenticator(audience string, keys oidc.KeySet) (*IAPAuthenticator, error) {
	parts := strings.Split(audience, "/")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "projects" || !decimal(parts[2]) || parts[3] != "global" || parts[4] != "backendServices" || !decimal(parts[5]) {
		return nil, fmt.Errorf("IAP audience must identify one global backend service by project number and service ID")
	}
	return &IAPAuthenticator{
		verifier: oidc.NewVerifier(iapIssuer, keys, &oidc.Config{
			ClientID:             audience,
			SupportedSigningAlgs: []string{"ES256"},
		}),
		audience: audience,
	}, nil
}

func (auth *IAPAuthenticator) Authenticate(ctx context.Context, assertion string) (Identity, error) {
	token, err := auth.verifier.Verify(ctx, assertion)
	if err != nil {
		return Identity{}, fmt.Errorf("invalid IAP assertion: %w", err)
	}
	var claims struct {
		Audience  string `json:"aud"`
		Email     string `json:"email"`
		IssuedAt  int64  `json:"iat"`
		ExpiresAt int64  `json:"exp"`
	}
	if err := token.Claims(&claims); err != nil {
		return Identity{}, fmt.Errorf("invalid IAP claims: %w", err)
	}
	now := time.Now().Unix()
	if claims.Audience != auth.audience || claims.IssuedAt <= 0 || claims.IssuedAt > now+30 || claims.ExpiresAt <= claims.IssuedAt || claims.ExpiresAt-claims.IssuedAt > 660 {
		return Identity{}, fmt.Errorf("invalid IAP audience or token lifetime")
	}
	address, err := mail.ParseAddress(claims.Email)
	if err != nil || address.Address != claims.Email || strings.Contains(claims.Email, ":") || token.Subject == "" {
		return Identity{}, fmt.Errorf("IAP assertion must contain a subject and a Google account email")
	}
	return Identity{Email: claims.Email, Subject: token.Subject}, nil
}

func decimal(value string) bool {
	if value == "" {
		return false
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}
