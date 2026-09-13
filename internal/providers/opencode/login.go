package opencode

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/mktcz/uniproxy/internal/protocols"
)

const publicAPIKey = "public"

type Credential struct {
	APIKey string `json:"api_key"`
}

func (c Credential) Validate() error {
	if strings.TrimSpace(c.APIKey) == "" {
		return fmt.Errorf("api_key is missing")
	}
	return nil
}

func (a *Adapter) Login(ctx context.Context) (Credential, error) {
	return a.LoginFrom(ctx, os.Stdin, os.Stdout)
}

func (a *Adapter) LoginFrom(ctx context.Context, in io.Reader, out io.Writer) (Credential, error) {
	if _, err := fmt.Fprint(out, "OpenCode API key: "); err != nil {
		return Credential{}, fmt.Errorf("write login prompt: %w", err)
	}
	lineCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(in)
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			errCh <- err
			return
		}
		lineCh <- line
	}()
	var raw string
	select {
	case <-ctx.Done():
		return Credential{}, ctx.Err()
	case err := <-errCh:
		return Credential{}, fmt.Errorf("read OpenCode API key: %w", err)
	case raw = <-lineCh:
	}
	key := strings.TrimSpace(raw)
	if key == "" {
		return Credential{}, fmt.Errorf("OpenCode API key is required")
	}
	credential := Credential{APIKey: key}
	if err := a.save(credential); err != nil {
		return Credential{}, err
	}
	a.mu.Lock()
	a.cache = &credential
	a.mu.Unlock()
	return credential, nil
}

func (a *Adapter) Check() error {
	_, err := a.loadOptional()
	return err
}

func (a *Adapter) loadOptional() (*Credential, error) {
	var credential Credential
	if err := a.creds.Get("opencode", &credential); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read OpenCode credential: %w", err)
	}
	if err := credential.Validate(); err != nil {
		return nil, fmt.Errorf("invalid OpenCode credential: %w", err)
	}
	return &credential, nil
}

func (a *Adapter) save(credential Credential) error {
	if err := credential.Validate(); err != nil {
		return fmt.Errorf("invalid OpenCode credential: %w", err)
	}
	if err := a.creds.Put("opencode", credential); err != nil {
		return fmt.Errorf("write OpenCode credential: %w", err)
	}
	return nil
}

func (a *Adapter) Logout() error {
	a.mu.Lock()
	a.cache = nil
	a.mu.Unlock()
	if err := a.creds.Delete("opencode"); err != nil {
		return fmt.Errorf("remove OpenCode credential: %w", err)
	}
	return nil
}

func (a *Adapter) resolveAPIKey(route Route) (string, error) {
	a.mu.Lock()
	cached := a.cache
	a.mu.Unlock()
	if cached == nil {
		loaded, err := a.loadOptional()
		if err != nil {
			return "", err
		}
		if loaded != nil {
			a.mu.Lock()
			a.cache = loaded
			a.mu.Unlock()
			cached = loaded
		}
	}
	if cached != nil && cached.APIKey != "" {
		return cached.APIKey, nil
	}
	if route.Plan == PlanGo {
		return "", &protocols.UpstreamError{
			Status:  http.StatusUnauthorized,
			Message: "OpenCode Go requires an API key; run 'uniproxy opencode login'",
			Code:    "missing_api_key",
		}
	}
	if !route.AllowAnonymous {
		return "", &protocols.UpstreamError{
			Status:  http.StatusUnauthorized,
			Message: "OpenCode model requires an API key; run 'uniproxy opencode login'",
			Code:    "missing_api_key",
		}
	}
	return publicAPIKey, nil
}
