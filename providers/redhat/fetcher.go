// Package redhat fetches the user's OpenShift pull secret from their Red Hat
// account: an OAuth 2.0 device authorization grant (RFC 8628) against Red
// Hat SSO, then one call to the OCM accounts API — the same API
// console.redhat.com serves the pull secret from. Self-contained per
// provider rules; stdlib HTTP only.
package redhat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/TheEasyShift/easyshift/interfaces"
)

const (
	// DefaultSSORealmURL is Red Hat's external SSO realm, the issuer used by
	// ocm/rosa device-code logins.
	DefaultSSORealmURL = "https://sso.redhat.com/auth/realms/redhat-external"
	// DefaultAPIURL is the OCM API base serving the account's pull secret.
	DefaultAPIURL = "https://api.openshift.com"
	// clientID is the public OAuth client for device-code logins — the same
	// one `ocm login --use-device-code` uses. Red Hat owns it; if it ever
	// changes, callers fall back to manual `pull-secret set`.
	clientID = "ocm-cli"

	deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"
	defaultInterval = 5 * time.Second
)

// slowDownIncrement is added to the polling interval on each slow_down
// response (RFC 8628 §3.5). Var, not const, so tests can shrink it.
var slowDownIncrement = 5 * time.Second

// Fetcher implements interfaces.PullSecretFetcher. Not safe for concurrent
// use: StartDeviceAuth stores the device-code state WaitAndFetch consumes.
type Fetcher struct {
	client      *http.Client
	ssoRealmURL string
	apiURL      string

	deviceCode string
	interval   time.Duration
	expiresAt  time.Time
}

// NewFetcher returns a Fetcher against the given SSO realm and OCM API base
// URLs (pass the Default* constants in production; tests pass an
// httptest.Server URL for both).
func NewFetcher(ssoRealmURL, apiURL string) *Fetcher {
	return &Fetcher{
		client:      &http.Client{Timeout: 30 * time.Second},
		ssoRealmURL: strings.TrimRight(ssoRealmURL, "/"),
		apiURL:      strings.TrimRight(apiURL, "/"),
	}
}

// StartDeviceAuth requests a device code and returns what to show the user.
func (f *Fetcher) StartDeviceAuth(ctx context.Context) (interfaces.DeviceAuthPrompt, error) {
	form := url.Values{"client_id": {clientID}}
	status, body, err := f.postForm(ctx, f.ssoRealmURL+"/protocol/openid-connect/auth/device", form)
	if err != nil {
		return interfaces.DeviceAuthPrompt{}, fmt.Errorf("request device code from Red Hat SSO: %w", err)
	}
	if status != http.StatusOK {
		return interfaces.DeviceAuthPrompt{}, fmt.Errorf("device-code request to Red Hat SSO returned HTTP %d: %s", status, body)
	}
	var resp struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		// Pointer distinguishes an explicit 0 (poll immediately, used by
		// tests) from an absent field (RFC 8628 default of 5s).
		Interval *int `json:"interval"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return interfaces.DeviceAuthPrompt{}, fmt.Errorf("parse device code response: %w", err)
	}
	f.deviceCode = resp.DeviceCode
	f.interval = defaultInterval
	if resp.Interval != nil {
		f.interval = time.Duration(*resp.Interval) * time.Second
	}
	f.expiresAt = time.Now().Add(time.Duration(resp.ExpiresIn) * time.Second)
	return interfaces.DeviceAuthPrompt{
		VerificationURI: resp.VerificationURI,
		UserCode:        resp.UserCode,
	}, nil
}

// WaitAndFetch polls the token endpoint until the user authorizes (or the
// code expires / ctx is cancelled), then fetches the pull secret.
func (f *Fetcher) WaitAndFetch(ctx context.Context) ([]byte, error) {
	if f.deviceCode == "" {
		return nil, fmt.Errorf("device login not started (StartDeviceAuth must be called first)")
	}
	token, err := f.pollToken(ctx)
	if err != nil {
		return nil, err
	}
	return f.fetchPullSecret(ctx, token)
}

func (f *Fetcher) pollToken(ctx context.Context) (string, error) {
	form := url.Values{
		"grant_type":  {deviceGrantType},
		"device_code": {f.deviceCode},
		"client_id":   {clientID},
	}
	for {
		status, body, err := f.postForm(ctx, f.ssoRealmURL+"/protocol/openid-connect/token", form)
		if err != nil {
			return "", fmt.Errorf("poll Red Hat SSO token endpoint: %w", err)
		}
		if status == http.StatusOK {
			var tok struct {
				AccessToken string `json:"access_token"`
			}
			if err := json.Unmarshal(body, &tok); err != nil || tok.AccessToken == "" {
				return "", fmt.Errorf("parse token response: %v (body: %s)", err, body)
			}
			return tok.AccessToken, nil
		}
		var oauthErr struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &oauthErr)
		switch oauthErr.Error {
		case "authorization_pending":
			// keep polling
		case "slow_down":
			f.interval += slowDownIncrement
		case "expired_token":
			return "", fmt.Errorf("the device code expired before the login was authorized; run the command again")
		case "access_denied":
			return "", fmt.Errorf("the login request was denied")
		default:
			return "", fmt.Errorf("token request to Red Hat SSO returned HTTP %d: %s", status, body)
		}
		if time.Now().After(f.expiresAt) {
			return "", fmt.Errorf("the device code expired before the login was authorized; run the command again")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(f.interval):
		}
	}
}

func (f *Fetcher) fetchPullSecret(ctx context.Context, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.apiURL+"/api/accounts_mgmt/v1/access_token", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch pull secret from %s: %w", f.apiURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read pull secret response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("OCM pull secret endpoint returned HTTP %d: %s", resp.StatusCode, body)
	}
	return body, nil
}

// postForm posts form data and returns the status code and full body. Any
// status is returned to the caller (token polling treats 400s as signals).
func (f *Fetcher) postForm(ctx context.Context, endpoint string, form url.Values) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := f.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, body, nil
}
