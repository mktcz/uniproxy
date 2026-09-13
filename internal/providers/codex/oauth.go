package codex

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mktcz/uniproxy/internal/utils/browser"
	"github.com/mktcz/uniproxy/internal/utils/randomid"
)

const (
	defaultAuthURL     = "https://auth.openai.com/oauth/authorize"
	defaultTokenURL    = "https://auth.openai.com/oauth/token"
	defaultClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	defaultRedirectURI = "http://localhost:1455/auth/callback"
	callbackAddress    = "127.0.0.1:1455"
)

const oauthCallbackPage = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>{{TITLE}}</title>
  <link rel="preconnect" href="https://fonts.googleapis.com" />
  <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin />
  <link href="https://fonts.googleapis.com/css2?family=IBM+Plex+Mono:wght@400;500&family=Instrument+Sans:wght@500;600;700&display=swap" rel="stylesheet" />
  <style>
    :root {
      --ink: #e8efe9;
      --muted: #8a9a8e;
      --bg0: #0b1210;
      --bg1: #122019;
      --line: rgba(232, 239, 233, 0.12);
      --ok: #6ee7a8;
      --bad: #ff8f7a;
      --glow: {{GLOW}};
    }
    * { box-sizing: border-box; }
    html { color-scheme: dark; }
    body {
      margin: 0;
      min-height: 100vh;
      display: grid;
      place-items: center;
      padding: 32px 20px;
      color: var(--ink);
      font-family: "Instrument Sans", ui-sans-serif, system-ui, sans-serif;
      background:
        radial-gradient(900px 520px at 50% -10%, var(--glow), transparent 60%),
        radial-gradient(700px 400px at 80% 110%, rgba(18, 40, 32, 0.9), transparent 55%),
        linear-gradient(165deg, var(--bg0), var(--bg1) 55%, #0a100e);
    }
    .shell {
      width: min(100%, 440px);
      border: 1px solid var(--line);
      border-radius: 4px;
      background: rgba(8, 14, 12, 0.72);
      backdrop-filter: blur(10px);
      padding: 28px 28px 24px;
      box-shadow: 0 24px 60px rgba(0, 0, 0, 0.35);
    }
    .brand {
      display: flex;
      align-items: center;
      gap: 10px;
      margin-bottom: 28px;
    }
    .mark {
      width: 28px;
      height: 28px;
      flex: none;
    }
    .brand-name {
      font-family: "IBM Plex Mono", ui-monospace, monospace;
      font-size: 13px;
      font-weight: 500;
      letter-spacing: 0.08em;
      text-transform: uppercase;
      color: var(--muted);
    }
    h1 {
      margin: 0 0 10px;
      font-size: clamp(1.6rem, 4vw, 2rem);
      line-height: 1.15;
      font-weight: 650;
      letter-spacing: -0.03em;
    }
    p {
      margin: 0;
      color: var(--muted);
      font-size: 15px;
      line-height: 1.65;
    }
    .details {
      margin-top: 18px;
      padding: 12px 14px;
      border-left: 2px solid color-mix(in srgb, {{ACCENT}} 55%, transparent);
      background: rgba(0, 0, 0, 0.28);
      font-family: "IBM Plex Mono", ui-monospace, monospace;
      font-size: 12px;
      line-height: 1.55;
      color: var(--muted);
      white-space: pre-wrap;
      word-break: break-word;
    }
    .footer {
      margin-top: 28px;
      padding-top: 16px;
      border-top: 1px solid var(--line);
      font-family: "IBM Plex Mono", ui-monospace, monospace;
      font-size: 11px;
      letter-spacing: 0.04em;
      color: color-mix(in srgb, var(--muted) 75%, transparent);
    }
  </style>
</head>
<body>
  <section class="shell">
    <div class="brand">
      <svg class="mark" viewBox="0 0 32 32" aria-hidden="true">
        <rect x="2" y="2" width="12" height="12" fill="#e8efe9"/>
        <rect x="18" y="2" width="12" height="28" fill="#6ee7a8"/>
        <rect x="2" y="18" width="12" height="12" fill="#8a9a8e"/>
      </svg>
      <span class="brand-name">UniProxy</span>
    </div>
    <h1>{{HEADING}}</h1>
    <p>{{MESSAGE}}</p>{{DETAILS}}
    <div class="footer">Return to your terminal to continue</div>
  </section>
</body>
</html>`

func renderOAuthCallback(success bool, message, details string) string {
	title, heading, status := "Signed in · UniProxy", "You're signed in", "Connected"
	glow, accent := "rgba(110, 231, 168, 0.22)", "var(--ok)"
	if !success {
		title, heading, status = "Sign-in failed · UniProxy", "Sign-in didn't finish", "Failed"
		glow, accent = "rgba(255, 143, 122, 0.20)", "var(--bad)"
	}
	page := oauthCallbackPage
	page = strings.ReplaceAll(page, "{{TITLE}}", html.EscapeString(title))
	page = strings.ReplaceAll(page, "{{HEADING}}", html.EscapeString(heading))
	page = strings.ReplaceAll(page, "{{MESSAGE}}", html.EscapeString(message))
	page = strings.ReplaceAll(page, "{{STATUS}}", html.EscapeString(status))
	page = strings.ReplaceAll(page, "{{GLOW}}", glow)
	page = strings.ReplaceAll(page, "{{ACCENT}}", accent)
	if details == "" {
		page = strings.ReplaceAll(page, "{{DETAILS}}", "")
	} else {
		page = strings.ReplaceAll(page, "{{DETAILS}}", "\n    <div class=\"details\">"+html.EscapeString(details)+"</div>")
	}
	return page
}

type OAuthClient struct {
	AuthURL         string
	TokenURL        string
	ClientID        string
	RedirectURI     string
	CallbackAddress string
	HTTPClient      *http.Client
	OpenBrowser     func(string) error
}

func NewOAuthClient(client *http.Client) *OAuthClient {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &OAuthClient{
		AuthURL: defaultAuthURL, TokenURL: defaultTokenURL, ClientID: defaultClientID,
		RedirectURI: defaultRedirectURI, CallbackAddress: callbackAddress,
		HTTPClient: client, OpenBrowser: browser.Open,
	}
}

func (o *OAuthClient) Login(ctx context.Context, noBrowser bool) (Credential, error) {
	verifier, err := randomid.URLSafe(96)
	if err != nil {
		return Credential{}, fmt.Errorf("generate PKCE verifier: %w", err)
	}
	state, err := randomid.URLSafe(32)
	if err != nil {
		return Credential{}, fmt.Errorf("generate OAuth state: %w", err)
	}
	challengeHash := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeHash[:])

	listener, err := net.Listen("tcp", o.CallbackAddress)
	if err != nil {
		return Credential{}, fmt.Errorf("start OAuth callback on %s: %w", o.CallbackAddress, err)
	}
	defer listener.Close()

	params := url.Values{
		"client_id": {o.ClientID}, "response_type": {"code"}, "redirect_uri": {o.RedirectURI},
		"scope": {"openid email profile offline_access"}, "state": {state},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "prompt": {"login"},
		"id_token_add_organizations": {"true"}, "codex_cli_simplified_flow": {"true"},
	}
	authURL := o.AuthURL + "?" + params.Encode()

	type callbackResult struct{ code, state, oauthError, description string }
	resultCh := make(chan callbackResult, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		result := callbackResult{
			code: r.URL.Query().Get("code"), state: r.URL.Query().Get("state"),
			oauthError: r.URL.Query().Get("error"), description: r.URL.Query().Get("error_description"),
		}
		select {
		case resultCh <- result:
		default:
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if result.oauthError != "" {
			details := result.oauthError
			if result.description != "" {
				details = result.oauthError + ": " + result.description
			}
			_, _ = io.WriteString(w, renderOAuthCallback(false, "Codex authentication failed. You can close this window and try again.", details))
			return
		}
		_, _ = io.WriteString(w, renderOAuthCallback(true, "Codex authentication completed. You can close this window.", ""))
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			serveErr <- err
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	fmt.Printf("Open this URL to authenticate Codex:\n%s\n", authURL)
	if !noBrowser && o.OpenBrowser != nil {
		if err := o.OpenBrowser(authURL); err != nil {
			fmt.Printf("Could not open a browser automatically: %v\n", err)
		}
	}
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var result callbackResult
	select {
	case <-waitCtx.Done():
		return Credential{}, fmt.Errorf("OAuth callback: %w", waitCtx.Err())
	case err := <-serveErr:
		return Credential{}, fmt.Errorf("OAuth callback server: %w", err)
	case result = <-resultCh:
	}
	if result.oauthError != "" {
		return Credential{}, fmt.Errorf("OAuth authorization failed: %s: %s", result.oauthError, result.description)
	}
	if result.code == "" || result.state == "" {
		return Credential{}, fmt.Errorf("OAuth callback is missing code or state")
	}
	if result.state != state {
		return Credential{}, fmt.Errorf("OAuth state mismatch")
	}
	return o.exchange(ctx, result.code, verifier)
}

func (o *OAuthClient) exchange(ctx context.Context, code, verifier string) (Credential, error) {
	values := url.Values{
		"grant_type": {"authorization_code"}, "client_id": {o.ClientID}, "code": {code},
		"redirect_uri": {o.RedirectURI}, "code_verifier": {verifier},
	}
	return o.tokenRequest(ctx, values, Credential{})
}

func (o *OAuthClient) Refresh(ctx context.Context, previous Credential) (Credential, error) {
	if previous.RefreshToken == "" {
		return Credential{}, fmt.Errorf("refresh token is missing")
	}
	values := url.Values{
		"client_id": {o.ClientID}, "grant_type": {"refresh_token"},
		"refresh_token": {previous.RefreshToken}, "scope": {"openid profile email"},
	}
	return o.tokenRequest(ctx, values, previous)
}

func (o *OAuthClient) tokenRequest(ctx context.Context, values url.Values, previous Credential) (Credential, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.TokenURL, strings.NewReader(values.Encode()))
	if err != nil {
		return Credential{}, fmt.Errorf("create token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := o.HTTPClient.Do(req)
	if err != nil {
		return Credential{}, fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Credential{}, fmt.Errorf("read token response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Credential{}, fmt.Errorf("token endpoint returned status %d", resp.StatusCode)
	}
	var wire struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return Credential{}, fmt.Errorf("decode token response: %w", err)
	}
	credential := previous
	credential.AccessToken = wire.AccessToken
	if wire.RefreshToken != "" {
		credential.RefreshToken = wire.RefreshToken
	}
	if wire.IDToken != "" {
		credential.IDToken = wire.IDToken
	}
	now := time.Now().UTC()
	credential.LastRefresh = now
	if wire.ExpiresIn > 0 {
		credential.ExpiresAt = now.Add(time.Duration(wire.ExpiresIn) * time.Second)
	}
	if claims, err := parseClaims(credential.IDToken); err == nil {
		if claims.AccountID != "" {
			credential.AccountID = claims.AccountID
		}
		if claims.Email != "" {
			credential.Email = claims.Email
		}
		if credential.ExpiresAt.IsZero() && claims.ExpiresAt > 0 {
			credential.ExpiresAt = time.Unix(claims.ExpiresAt, 0).UTC()
		}
	}
	if err := credential.Validate(); err != nil {
		return Credential{}, fmt.Errorf("token response is incomplete: %w", err)
	}
	return credential, nil
}

type tokenClaims struct {
	Email     string `json:"email"`
	ExpiresAt int64  `json:"exp"`
	Auth      struct {
		AccountID string `json:"chatgpt_account_id"`
	} `json:"https://api.openai.com/auth"`
	AccountID string `json:"-"`
}

func parseClaims(token string) (tokenClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return tokenClaims{}, fmt.Errorf("invalid ID token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return tokenClaims{}, err
	}
	var claims tokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return tokenClaims{}, err
	}
	claims.AccountID = claims.Auth.AccountID
	return claims, nil
}
