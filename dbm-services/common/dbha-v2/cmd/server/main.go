package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"dbm-services/common/dbha-v2/internal/server"
	"github.com/spf13/cobra"
)

func run(ctx context.Context, args []string) error {
	var configFile, base, bodyFile, ifMatch, ifNoneMatch, idempotencyKey string
	var bases []string
	root := &cobra.Command{Use: "dbha-server", Short: "DBHA unified control service", SilenceUsage: true, SilenceErrors: true, RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := server.LoadConfig(configFile)
		if err != nil {
			return err
		}
		return server.Run(cmd.Context(), cfg)
	}}
	root.PersistentFlags().StringVarP(&configFile, "config", "c", "/etc/dbha/server.json", "local server configuration")
	ctl := &cobra.Command{Use: "ctl METHOD /api/v1/path", Short: "Call the authenticated management API", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := server.LoadConfig(configFile)
		if err != nil {
			return err
		}
		var body []byte
		if bodyFile != "" {
			body, err = os.ReadFile(bodyFile)
			if err != nil {
				return err
			}
		}
		headers := map[string]string{}
		if ifMatch != "" {
			headers["If-Match"] = ifMatch
		}
		if ifNoneMatch != "" {
			headers["If-None-Match"] = ifNoneMatch
		}
		if idempotencyKey != "" {
			headers["Idempotency-Key"] = idempotencyKey
		}
		if base != "" {
			if len(bases) > 0 {
				return fmt.Errorf("use either --server or --servers")
			}
			bases = []string{base}
		}
		out, err := server.ControlRequestAny(cmd.Context(), cfg, bases, args[0], args[1], body, headers)
		if len(out) > 0 {
			fmt.Fprintln(cmd.OutOrStdout(), string(out))
		}
		return err
	}}
	ctl.Flags().StringVar(&base, "server", "", "HTTP or HTTPS server URL")
	ctl.Flags().StringSliceVar(&bases, "servers", nil, "HTTP or HTTPS server URLs tried in order")
	ctl.Flags().StringVar(&bodyFile, "data-file", "", "JSON request body file")
	ctl.Flags().StringVar(&ifMatch, "if-match", "", "resource revision")
	ctl.Flags().StringVar(&ifNoneMatch, "if-none-match", "", "use * for create-only")
	ctl.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "deployment installation key")
	root.AddCommand(ctl)
	for _, action := range []string{"prepare-restore", "reset-admin-token"} {
		root.AddCommand(&cobra.Command{Use: action, Short: "Offline maintenance; requires controller stopped", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := server.LoadConfig(configFile)
			if err != nil {
				return err
			}
			return server.OfflineControl(cmd.Context(), cfg, cmd.Name())
		}})
	}
	root.SetArgs(args)
	return root.ExecuteContext(ctx)
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
