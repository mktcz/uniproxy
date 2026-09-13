package codex

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestLoginUsesPKCEAndValidatesCallback(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"login@example.com","https://api.openai.com/auth":{"chatgpt_account_id":"acct-login"}}`))
	idToken := "e30." + payload + ".sig"
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code_verifier") == "" {
			t.Errorf("missing authorization-code PKCE fields: %#v", r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":"access","refresh_token":"refresh","id_token":%q,"expires_in":3600}`, idToken)
	}))
	defer tokenServer.Close()

	callbackAddress := unusedAddress(t)
	oauth := NewOAuthClient(tokenServer.Client())
	oauth.AuthURL = "https://auth.example/authorize"
	oauth.TokenURL = tokenServer.URL
	oauth.CallbackAddress = callbackAddress
	oauth.RedirectURI = "http://" + callbackAddress + "/auth/callback"
	oauth.OpenBrowser = func(target string) error {
		parsed, err := url.Parse(target)
		if err != nil {
			return err
		}
		query := parsed.Query()
		if query.Get("code_challenge") == "" || query.Get("code_challenge_method") != "S256" {
			t.Errorf("missing PKCE challenge: %s", target)
		}
		callback := oauth.RedirectURI + "?code=code-1&state=" + url.QueryEscape(query.Get("state"))
		response, err := http.Get(callback)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, response.Body)
		return response.Body.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	credential, err := oauth.Login(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if credential.AccountID != "acct-login" || credential.Email != "login@example.com" || credential.AccessToken != "access" {
		t.Fatalf("unexpected credential: %#v", credential)
	}
}

func TestLoginRejectsStateMismatchWithoutTokenExchange(t *testing.T) {
	callbackAddress := unusedAddress(t)
	oauth := NewOAuthClient(&http.Client{Timeout: time.Second})
	oauth.AuthURL = "https://auth.example/authorize"
	oauth.CallbackAddress = callbackAddress
	oauth.RedirectURI = "http://" + callbackAddress + "/auth/callback"
	oauth.OpenBrowser = func(string) error {
		response, err := http.Get(oauth.RedirectURI + "?code=code-1&state=wrong")
		if err == nil {
			_ = response.Body.Close()
		}
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := oauth.Login(ctx, false); err == nil {
		t.Fatal("expected state mismatch")
	}
}

func unusedAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}
