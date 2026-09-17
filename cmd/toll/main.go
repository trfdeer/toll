// Command toll is a minimal, self-hosted LLM gateway in front of any
// OpenAI-compatible API.
//
// 12-factor: configuration comes from the environment (plus a declarative
// toll.yaml for upstreams/rules — secrets always via env); logs stream to
// stdout; state lives in SQLite under TOLL_DATA_DIR.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/fang"
	"github.com/charmbracelet/log"
	"github.com/spf13/cobra"

	"github.com/trfdeer/toll/internal/config"
	"github.com/trfdeer/toll/internal/discovery"
	"github.com/trfdeer/toll/internal/keys"
	"github.com/trfdeer/toll/internal/server"
	"github.com/trfdeer/toll/internal/store"
)

var version = "dev"

func main() {
	var flags config.Flags

	cmd := &cobra.Command{
		Use:   "toll",
		Short: "A minimal, self-hosted LLM gateway for OpenAI-compatible APIs",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cmd.Context(), flags)
			if err != nil {
				return err
			}
			return run(cmd.Context(), cfg)
		},
	}

	cmd.Flags().StringVarP(&flags.Listen, "listen", "l", "", "listen address (env: TOLL_LISTEN)")
	cmd.Flags().StringVarP(&flags.Config, "config", "c", "", "config file (env: TOLL_CONFIG, default ./toll.yaml if present)")

	keysCmd := &cobra.Command{
		Use:   "keys",
		Short: "Manage virtual API keys",
	}
	keysCmd.AddCommand(keysAddCmd(), keysListCmd(), keysRevokeCmd())
	cmd.AddCommand(keysCmd)
	cmd.AddCommand(healthcheckCmd())

	if err := fang.Execute(context.Background(), cmd, fang.WithVersion(version)); err != nil {
		os.Exit(1)
	}
}

// healthcheckCmd probes a running gateway's /healthz. Used by the Docker
// HEALTHCHECK (distroless images have no shell/wget).
func healthcheckCmd() *cobra.Command {
	var url string
	cmd := &cobra.Command{
		Use:   "healthcheck",
		Short: "Probe the gateway health endpoint (for container health checks)",
		RunE: func(cmd *cobra.Command, args []string) error {
			client := &http.Client{Timeout: 3 * time.Second}
			resp, err := client.Get(url)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("health check failed: status %d", resp.StatusCode)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&url, "url", "http://127.0.0.1:8080/healthz", "health endpoint to probe")
	return cmd
}

// openStoreForCmd opens the store for management subcommands (which do not
// need upstreams configured).
func openStoreForCmd() (*store.Store, func(), error) {
	db, err := store.Open(filepath.Join(config.DataDirOnly().DataDir, "toll.db"))
	if err != nil {
		return nil, nil, err
	}
	return db, func() { db.Close() }, nil
}

func keysAddCmd() *cobra.Command {
	var allowProviders, denyProviders, allowModels, denyModels []string
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Create a virtual API key (plaintext shown once)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, cleanup, err := openStoreForCmd()
			if err != nil {
				return err
			}
			defer cleanup()

			provider, err := filterFromFlags("provider", allowProviders, denyProviders)
			if err != nil {
				return err
			}
			model, err := filterFromFlags("model", allowModels, denyModels)
			if err != nil {
				return err
			}

			plaintext, hash, err := keys.Generate()
			if err != nil {
				return err
			}
			if _, err := db.CreateVirtualKey(cmd.Context(), args[0], hash, provider, model); err != nil {
				return err
			}
			logger := log.New(os.Stdout)
			logger.Info("key created (store this now — it is not recoverable)", "name", args[0], "key", plaintext)
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&allowProviders, "allow-provider", nil, "only these providers are usable")
	cmd.Flags().StringSliceVar(&denyProviders, "deny-provider", nil, "these providers are unusable")
	cmd.Flags().StringSliceVar(&allowModels, "allow-model", nil, "only these gateway model IDs are usable")
	cmd.Flags().StringSliceVar(&denyModels, "deny-model", nil, "these gateway model IDs are unusable")
	return cmd
}

// filterFromFlags builds an include filter from allow, or an exclude filter
// from deny. Supplying both is a mistake.
func filterFromFlags(label string, allow, deny []string) (store.KeyFilter, error) {
	switch {
	case len(allow) > 0 && len(deny) > 0:
		return store.KeyFilter{}, fmt.Errorf("pass either --allow-%s or --deny-%s, not both", label, label)
	case len(allow) > 0:
		return store.KeyFilter{Mode: "include", Values: allow}, nil
	case len(deny) > 0:
		return store.KeyFilter{Mode: "exclude", Values: deny}, nil
	default:
		return store.KeyFilter{Mode: "none"}, nil
	}
}

func describeFilter(label string, f store.KeyFilter) string {
	if f.Mode == "none" || f.Mode == "" {
		return label + ": all"
	}
	return label + " " + f.Mode + ": " + strings.Join(f.Values, ",")
}

func keysListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List virtual API keys",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, cleanup, err := openStoreForCmd()
			if err != nil {
				return err
			}
			defer cleanup()
			vks, err := db.ListVirtualKeys(cmd.Context())
			if err != nil {
				return err
			}
			logger := log.New(os.Stdout)
			for _, vk := range vks {
				logger.Info("key", "name", vk.Name,
					"providers", describeFilter("provider", vk.ProviderFilter),
					"models", describeFilter("model", vk.ModelFilter))
			}
			return nil
		},
	}
}

func keysRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <name>",
		Short: "Revoke a virtual API key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, cleanup, err := openStoreForCmd()
			if err != nil {
				return err
			}
			defer cleanup()
			if err := db.RevokeVirtualKey(cmd.Context(), args[0]); err != nil {
				return err
			}
			log.New(os.Stdout).Info("key revoked", "name", args[0])
			return nil
		},
	}
}

func run(ctx context.Context, cfg *config.Config) error {
	// The run context is cancelled on shutdown, stopping background
	// workers (discovery syncer) first, then the HTTP server.
	ctx, stop := context.WithCancel(ctx)
	defer stop()

	logger := log.NewWithOptions(os.Stdout, log.Options{
		Level:           cfg.LogLevel,
		ReportTimestamp: true,
	})

	db, err := store.Open(filepath.Join(cfg.DataDir, "toll.db"))
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer db.Close()
	logger.Info("store ready", "data_dir", cfg.DataDir)

	// An explicit config/env value for prompt storage seeds the DB setting;
	// otherwise the DB (and its UI toggle) governs.
	if cfg.StorePrompts != nil {
		if err := db.SetSetting(ctx, store.SettingStorePrompts, strconv.FormatBool(*cfg.StorePrompts)); err != nil {
			return fmt.Errorf("apply store_prompts: %w", err)
		}
		logger.Info("prompt storage configured", "store_prompts", *cfg.StorePrompts)
	}

	for _, u := range cfg.Upstreams {
		logger.Info("upstream configured", "name", u.Name, "url", u.URL.String(),
			"refresh", u.Refresh, "disabled_refresh", u.DisableRefresh)
	}
	if len(cfg.Upstreams) == 0 {
		logger.Warn("no upstreams configured; add providers via the admin UI")
	}

	// Model registry sync: startup + periodic per upstream.
	syncer := discovery.NewSyncer(db, cfg, logger)
	go syncer.Run(ctx)

	srv := server.New(cfg, db, logger)

	errCh := make(chan error, 1)
	go func() {
		logger.Info("toll listening", "addr", cfg.Listen)
		errCh <- srv.ListenAndServe()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		logger.Info("shutting down", "signal", sig.String())
		stop()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
