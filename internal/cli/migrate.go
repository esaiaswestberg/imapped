package cli

import (
	"context"
	"fmt"
	"text/tabwriter"

	"github.com/esaiaswestberg/imapped/internal/app"
	"github.com/esaiaswestberg/imapped/internal/db"
	"github.com/spf13/cobra"
)

func newMigrateCommand(opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Apply database migrations",
		Long: "Applies any pending migrations. The server does this on startup unless\n" +
			"db.auto_migrate is disabled, so this is mainly for deployments that\n" +
			"prefer to migrate as a separate step.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withApp(cmd.Context(), opts, func(ctx context.Context, a *app.App) error {
				if err := db.Migrate(ctx, a.Pool, a.Log); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "database schema is up to date")
				return nil
			})
		},
	}
	cmd.AddCommand(newMigrateStatusCommand(opts))
	return cmd
}

func newMigrateStatusCommand(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show which migrations have been applied",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withApp(cmd.Context(), opts, func(ctx context.Context, a *app.App) error {
				statuses, err := db.Status(ctx, a.Pool)
				if err != nil {
					return err
				}
				tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "VERSION\tNAME\tAPPLIED")
				for _, s := range statuses {
					applied := "pending"
					if s.Applied {
						applied = s.AppliedAt.Local().Format("2006-01-02 15:04")
					}
					fmt.Fprintf(tw, "%d\t%s\t%s\n", s.Version, s.Name, applied)
				}
				return tw.Flush()
			})
		},
	}
}
