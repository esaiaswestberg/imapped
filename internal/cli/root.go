// Package cli assembles the command tree for the imapped binary.
package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// Build metadata, injected via -ldflags at release time.
var (
	Version = "dev"
	Commit  = "none"
)

// options are the flags shared by every subcommand.
type options struct {
	configPath string
}

// NewRootCommand builds the command tree. Output is written to the returned
// command's streams so tests can capture it.
func NewRootCommand() *cobra.Command {
	opts := &options{}

	root := &cobra.Command{
		Use:   "imapped",
		Short: "An IMAP caching proxy with a web UI",
		Long: "imapped mirrors upstream IMAP accounts into local storage, serves them\n" +
			"to mail clients over IMAP, and exposes sync and administration through a\n" +
			"web interface.",
		SilenceUsage: true,
		// Errors are printed by main with full formatting; cobra printing them
		// too would duplicate multi-line validation output.
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVarP(&opts.configPath, "config", "c", "",
		"path to a TOML configuration file (default: ./imapped.toml, /etc/imapped/config.toml)")

	root.AddCommand(
		newRunCommand(opts),
		newConfigCommand(opts),
		newVersionCommand(),
	)
	return root
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return printVersion(cmd.OutOrStdout())
		},
	}
}

func printVersion(w io.Writer) error {
	_, err := fmt.Fprintf(w, "imapped %s (%s)\n", Version, Commit)
	return err
}
