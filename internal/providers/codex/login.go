package codex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

type Credential struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	IDToken      string    `json:"id_token"`
	AccountID    string    `json:"account_id"`
	Email        string    `json:"email,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	LastRefresh  time.Time `json:"last_refresh"`
}

func (c Credential) Validate() error {
	if c.AccessToken == "" {
		return fmt.Errorf("access_token is missing")
	}
	if c.RefreshToken == "" {
		return fmt.Errorf("refresh_token is missing")
	}
	if c.AccountID == "" {
		return fmt.Errorf("account_id is missing")
	}
	return nil
}

func (a *Adapter) Login(ctx context.Context, noBrowser bool) (Credential, error) {
	credential, err := a.oauth.Login(ctx, noBrowser)
	if err != nil {
		return Credential{}, err
	}
	if err := a.save(credential); err != nil {
		return Credential{}, err
	}
	a.mu.Lock()
	a.cache = &credential
	a.mu.Unlock()
	return credential, nil
}

func (a *Adapter) Check() error {
	_, err := a.load()
	return err
}

func (a *Adapter) token(ctx context.Context) (Credential, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	credential := Credential{}
	if a.cache != nil {
		credential = *a.cache
	} else {
		loaded, err := a.load()
		if err != nil {
			return Credential{}, err
		}
		credential = loaded
	}
	if credential.ExpiresAt.IsZero() || credential.ExpiresAt.After(a.now().Add(5*time.Minute)) {
		a.cache = &credential
		return credential, nil
	}
	refreshed, err := a.oauth.Refresh(ctx, credential)
	if err != nil {
		return Credential{}, fmt.Errorf("refresh Codex credential: %w", err)
	}
	if err := a.save(refreshed); err != nil {
		return Credential{}, err
	}
	a.cache = &refreshed
	return refreshed, nil
}

func (a *Adapter) load() (Credential, error) {
	var credential Credential
	if err := a.creds.Get("codex", &credential); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Credential{}, fmt.Errorf("Codex credential not found; run 'uniproxy codex login': %w", err)
		}
		return Credential{}, fmt.Errorf("read Codex credential: %w", err)
	}
	if err := credential.Validate(); err != nil {
		return Credential{}, fmt.Errorf("invalid Codex credential: %w", err)
	}
	return credential, nil
}

func (a *Adapter) save(credential Credential) error {
	if err := credential.Validate(); err != nil {
		return fmt.Errorf("invalid Codex credential: %w", err)
	}
	if err := a.creds.Put("codex", credential); err != nil {
		return fmt.Errorf("save Codex credential: %w", err)
	}
	return nil
}

func (a *Adapter) Logout() error {
	a.mu.Lock()
	a.cache = nil
	a.mu.Unlock()
	if err := a.creds.Delete("codex"); err != nil {
		return fmt.Errorf("remove Codex credential: %w", err)
	}
	return nil
}
