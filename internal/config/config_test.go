package config

import (
	"path/filepath"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("LISTEN_ADDRESS", "")
	t.Setenv("AUTH_FILE", "")
	t.Setenv("LOG_LEVEL", "")

	cfg := Defaults()
	if cfg.ListenAddress != DefaultListenAddress {
		t.Fatalf("listen = %q", cfg.ListenAddress)
	}
	if cfg.IdleTimeout != DefaultIdleTimeout {
		t.Fatalf("idle timeout = %s", cfg.IdleTimeout)
	}
	wantAuth := filepath.Join(home, ".config", "uniproxy", "auth.json")
	if cfg.AuthFile != wantAuth {
		t.Fatalf("auth file = %s, want %s", cfg.AuthFile, wantAuth)
	}
}

func TestDefaultsFromEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("LISTEN_ADDRESS", "localhost:9000")
	t.Setenv("IDLE_TIMEOUT", "90s")
	t.Setenv("LOG_LEVEL", "DEBUG")
	t.Setenv("AUTH_FILE", filepath.Join(home, "auth.json"))

	cfg := Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddress != "localhost:9000" {
		t.Fatalf("unexpected config: %#v", cfg)
	}
	if cfg.IdleTimeout != 90*time.Second {
		t.Fatalf("idle timeout = %s", cfg.IdleTimeout)
	}
	if cfg.LogLevel != "debug" {
		t.Fatalf("log level = %q", cfg.LogLevel)
	}
	if cfg.AuthFile != filepath.Join(home, "auth.json") {
		t.Fatalf("auth file = %s", cfg.AuthFile)
	}
}

func TestValidateRejectsNonLoopback(t *testing.T) {
	cfg := Defaults()
	cfg.ListenAddress = "0.0.0.0:6917"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected listen address error")
	}
}

func TestValidateExpandsAuthHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	cfg := Defaults()
	cfg.AuthFile = "~/.local/uniproxy-auth.json"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.AuthFile != filepath.Join(home, ".local", "uniproxy-auth.json") {
		t.Fatalf("auth path = %s", cfg.AuthFile)
	}
}
