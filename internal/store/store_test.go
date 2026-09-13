package store_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/mktcz/uniproxy/internal/store"
)

type sample struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

func TestStoreUsesPrivatePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "auth.json")
	s := store.New(path)
	if err := s.Put("codex", sample{AccessToken: "access"}); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %o", info.Mode().Perm())
		}
	}
}

func TestStoreKeepsProvidersSeparate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	s := store.New(path)

	if err := s.Put("codex", sample{AccessToken: "c-access", RefreshToken: "c-refresh"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("cursor", sample{AccessToken: "u-access", RefreshToken: "u-refresh"}); err != nil {
		t.Fatal(err)
	}

	var codex sample
	if err := s.Get("codex", &codex); err != nil {
		t.Fatal(err)
	}
	if codex.AccessToken != "c-access" {
		t.Fatalf("codex = %#v", codex)
	}

	var cursor sample
	if err := s.Get("cursor", &cursor); err != nil {
		t.Fatal(err)
	}
	if cursor.AccessToken != "u-access" {
		t.Fatalf("cursor = %#v", cursor)
	}

	if err := s.Put("codex", sample{AccessToken: "c-new", RefreshToken: "c-refresh"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Get("cursor", &cursor); err != nil || cursor.AccessToken != "u-access" {
		t.Fatalf("cursor clobbered: %#v %v", cursor, err)
	}
}

func TestStoreMissingProvider(t *testing.T) {
	s := store.New(filepath.Join(t.TempDir(), "auth.json"))
	var got sample
	if err := s.Get("codex", &got); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want not exist", err)
	}
}

func TestStoreDeleteProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	s := store.New(path)
	if err := s.Put("codex", sample{AccessToken: "c-access", RefreshToken: "c-refresh"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("opencode", sample{AccessToken: "o-key"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("codex"); err != nil {
		t.Fatal(err)
	}
	var got sample
	if err := s.Get("codex", &got); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("codex after delete: %v", err)
	}
	if err := s.Get("opencode", &got); err != nil || got.AccessToken != "o-key" {
		t.Fatalf("opencode clobbered: %#v %v", got, err)
	}
	if err := s.Delete("codex"); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
}
