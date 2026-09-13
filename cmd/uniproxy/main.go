package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/mktcz/uniproxy/internal/config"
	"github.com/mktcz/uniproxy/internal/protocols"
	"github.com/mktcz/uniproxy/internal/protocols/anthropic"
	"github.com/mktcz/uniproxy/internal/protocols/openai"
	"github.com/mktcz/uniproxy/internal/providers"
	"github.com/mktcz/uniproxy/internal/providers/codex"
	"github.com/mktcz/uniproxy/internal/providers/cursor"
	"github.com/mktcz/uniproxy/internal/providers/opencode"
	"github.com/mktcz/uniproxy/internal/server"
	"github.com/spf13/cobra"
)

func main() {
	cfg := config.Defaults()

	root := &cobra.Command{
		Use:           "uniproxy",
		Short:         "Universal LLM protocol proxy",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	var noBrowser bool

	codexCmd := &cobra.Command{
		Use:   "codex",
		Short: "Codex provider commands",
	}
	codexLogin := &cobra.Command{
		Use:   "login",
		Short: "Authenticate with Codex via OAuth",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := cfg.Validate(); err != nil {
				return err
			}
			credential, err := codex.New(cfg.AuthFile, httpClient(cfg)).Login(cmd.Context(), noBrowser)
			if err != nil {
				return err
			}
			if credential.Email != "" {
				fmt.Printf("Codex authentication saved for %s\n", credential.Email)
			} else {
				fmt.Println("Codex authentication saved")
			}
			return nil
		},
	}
	codexLogin.Flags().BoolVar(&noBrowser, "no-browser", false, "print the login URL without opening a browser")
	codexCmd.AddCommand(codexLogin, &cobra.Command{
		Use:   "logout",
		Short: "Remove saved Codex credentials",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := cfg.Validate(); err != nil {
				return err
			}
			if err := codex.New(cfg.AuthFile, httpClient(cfg)).Logout(); err != nil {
				return err
			}
			fmt.Println("Codex credentials removed")
			return nil
		},
	})

	opencodeCmd := &cobra.Command{
		Use:   "opencode",
		Short: "OpenCode provider commands",
	}

	opencodeCmd.AddCommand(
		&cobra.Command{
			Use:   "login",
			Short: "Authenticate with OpenCode via API key",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				if err := cfg.Validate(); err != nil {
					return err
				}
				if _, err := opencode.New(cfg.AuthFile, httpClient(cfg)).Login(cmd.Context()); err != nil {
					return err
				}
				fmt.Println("OpenCode authentication saved")
				return nil
			},
		},
		&cobra.Command{
			Use:   "logout",
			Short: "Remove saved OpenCode credentials",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				if err := cfg.Validate(); err != nil {
					return err
				}
				if err := opencode.New(cfg.AuthFile, httpClient(cfg)).Logout(); err != nil {
					return err
				}
				fmt.Println("OpenCode credentials removed")
				return nil
			},
		})

	serve := &cobra.Command{
		Use:   "serve",
		Short: "Start the proxy server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := cfg.Validate(); err != nil {
				fmt.Fprintln(os.Stderr, "invalid configuration:", err)
				return err
			}

			logLevel := func(level string) slog.Level {
				switch level {
				case "debug":
					return slog.LevelDebug
				case "warn":
					return slog.LevelWarn
				case "error":
					return slog.LevelError
				default:
					return slog.LevelInfo
				}
			}

			logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel(cfg.LogLevel)}))

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			providers := providers.NewRegistry()
			providers.Register(codex.New(cfg.AuthFile, httpClient(cfg)))
			providers.Register(cursor.New())
			providers.Register(opencode.New(cfg.AuthFile, httpClient(cfg)))

			protocols := protocols.NewRegistry()
			protocols.Register(openai.Codec{})
			protocols.Register(anthropic.Codec{})

			server := server.New(
				cfg.ListenAddress,
				cfg.ReadHeaderTimeout,
				cfg.IdleTimeout,
				providers,
				protocols,
				logger,
			)

			var once sync.Once
			var result error
			shutdown := func() error {
				once.Do(func() {
					shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
					defer cancel()
					if err := server.Shutdown(shutdownCtx); err != nil {
						result = errors.Join(result, fmt.Errorf("shutdown HTTP server: %w", err))
					}
				})
				return result
			}

			listener, err := net.Listen("tcp", cfg.ListenAddress)
			if err != nil {
				return errors.Join(
					fmt.Errorf("listen on %s: %w", cfg.ListenAddress, err),
					shutdown(),
				)
			}

			logger.Info(
				"server started",
				"address", listener.Addr().String(),
				"providers", providers.Names(),
			)

			done := make(chan error, 1)
			go func() {
				<-ctx.Done()
				logger.Info("server shutting down")
				done <- shutdown()
			}()

			serveErr := server.Serve(listener)
			if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				return errors.Join(
					fmt.Errorf("serve proxy: %w", serveErr),
					shutdown(),
				)
			}

			if err := <-done; err != nil {
				logger.Error("server shutdown failed", "error", err)
				return err
			}
			return nil
		},
	}

	root.AddCommand(codexCmd, opencodeCmd, serve)

	root.PersistentFlags().StringVar(&cfg.AuthFile, "auth-file", cfg.AuthFile, "path to shared provider credentials file")
	root.PersistentFlags().DurationVar(&cfg.ResponseHeaderTimeout, "response-header-timeout", cfg.ResponseHeaderTimeout, "upstream response header timeout")
	root.PersistentFlags().StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "log level (debug, info, warn, error)")

	serve.Flags().StringVar(&cfg.ListenAddress, "listen", cfg.ListenAddress, "HTTP listen address")
	serve.Flags().DurationVar(&cfg.ReadHeaderTimeout, "read-header-timeout", cfg.ReadHeaderTimeout, "HTTP read header timeout")
	serve.Flags().DurationVar(&cfg.IdleTimeout, "idle-timeout", cfg.IdleTimeout, "HTTP idle timeout")
	serve.Flags().DurationVar(&cfg.ShutdownTimeout, "shutdown-timeout", cfg.ShutdownTimeout, "graceful shutdown timeout")

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func httpClient(cfg *config.Config) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: cfg.ResponseHeaderTimeout,
			IdleConnTimeout:       90 * time.Second,
			DisableCompression:    true,
		},
	}
}
