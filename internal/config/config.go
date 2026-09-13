package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
)

const (
	DefaultListenAddress         = "127.0.0.1:6917"
	DefaultReadHeaderTimeout     = 10 * time.Second
	DefaultIdleTimeout           = 120 * time.Second
	DefaultShutdownTimeout       = 10 * time.Second
	DefaultResponseHeaderTimeout = 60 * time.Second
	DefaultLogLevel              = "info"
)

type Config struct {
	ListenAddress         string        `validate:"required"`
	ReadHeaderTimeout     time.Duration `validate:"required,gte=1s"`
	IdleTimeout           time.Duration `validate:"required,gte=1s"`
	ShutdownTimeout       time.Duration `validate:"required,gte=1s,lte=300s"`
	AuthFile              string        `validate:"required"`
	ResponseHeaderTimeout time.Duration `validate:"required,gt=0"`
	LogLevel              string        `validate:"required,oneof=debug info warn error"`
}

func Defaults() *Config {
	return &Config{
		ListenAddress:         env("LISTEN_ADDRESS", DefaultListenAddress),
		ReadHeaderTimeout:     env("READ_HEADER_TIMEOUT", DefaultReadHeaderTimeout),
		IdleTimeout:           env("IDLE_TIMEOUT", DefaultIdleTimeout),
		ShutdownTimeout:       env("SHUTDOWN_TIMEOUT", DefaultShutdownTimeout),
		AuthFile:              env("AUTH_FILE", defaultAuthFile()),
		ResponseHeaderTimeout: env("RESPONSE_HEADER_TIMEOUT", DefaultResponseHeaderTimeout),
		LogLevel:              strings.ToLower(env("LOG_LEVEL", DefaultLogLevel)),
	}
}

func (c *Config) Validate() error {
	c.ListenAddress = strings.TrimSpace(c.ListenAddress)
	c.LogLevel = strings.ToLower(strings.TrimSpace(c.LogLevel))

	authFile, err := ExpandPath(c.AuthFile)
	if err != nil {
		return fmt.Errorf("auth file: %w", err)
	}
	c.AuthFile = authFile

	if err := validator.New(validator.WithRequiredStructEnabled()).Struct(c); err != nil {
		return fmt.Errorf("config validation failed: %w", err)
	}
	if err := validateLoopbackAddress(c.ListenAddress); err != nil {
		return err
	}
	return nil
}

func ExpandPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		if path == "~" {
			return home, nil
		}
		path = filepath.Join(home, path[2:])
	}
	return filepath.Clean(path), nil
}

func validateLoopbackAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("listen address must be host:port: %w", err)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("listen address must use a loopback host")
	}
	return nil
}

func defaultAuthFile() string {
	base, err := configHome()
	if err != nil {
		return ""
	}
	return filepath.Join(base, "uniproxy", "auth.json")
}

func configHome() (string, error) {
	if value := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); value != "" {
		return filepath.Clean(value), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config"), nil
}

func env[T string | time.Duration](key string, fallback T) T {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	switch any(fallback).(type) {
	case string:
		return any(v).(T)
	case time.Duration:
		if d, err := time.ParseDuration(v); err == nil {
			return any(d).(T)
		}
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			return any(time.Duration(secs) * time.Second).(T)
		}
	}
	return fallback
}
