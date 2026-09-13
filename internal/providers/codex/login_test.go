package codex

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestAuthRefreshesOnceAndPersistsRotation(t *testing.T) {
	var calls atomic.Int32
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"a@example.com","https://api.openai.com/auth":{"chatgpt_account_id":"acct-new"}}`))
	idToken := "e30." + payload + ".sig"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "refresh_token" {
			t.Errorf("unexpected refresh form: %v %#v", err, r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":"new-access","refresh_token":"new-refresh","id_token":%q,"expires_in":3600}`, idToken)
	}))
	defer server.Close()

	adapter := New(filepath.Join(t.TempDir(), "auth.json"), server.Client())
	adapter.oauth.TokenURL = server.URL
	if err := adapter.save(Credential{AccessToken: "old", RefreshToken: "old-refresh", AccountID: "acct", ExpiresAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	credential, err := adapter.token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if credential.AccessToken != "new-access" || credential.RefreshToken != "new-refresh" || credential.AccountID != "acct-new" {
		t.Fatalf("unexpected refreshed credential: %#v", credential)
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh calls = %d", calls.Load())
	}
	loaded, err := adapter.load()
	if err != nil || loaded.RefreshToken != "new-refresh" {
		t.Fatalf("rotation not persisted: %#v %v", loaded, err)
	}
}
