package cli

import (
	"fmt"
	"io"
	"sort"
	"text/tabwriter"

	"github.com/esaiaswestberg/imapped/internal/config"
	"github.com/spf13/cobra"
)

func newConfigCommand(opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect configuration",
	}
	cmd.AddCommand(newConfigCheckCommand(opts), newConfigShowCommand(opts))
	return cmd
}

func newConfigCheckCommand(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Validate configuration and exit",
		Long: "Loads configuration exactly as `run` would and reports every problem found.\n" +
			"Exits non-zero if the configuration would prevent startup, which makes it\n" +
			"usable as a container healthcheck or a CI gate.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if res.FileLoaded {
				fmt.Fprintf(out, "configuration file: %s\n", res.FilePath)
			} else {
				fmt.Fprintln(out, "configuration file: none (defaults and environment only)")
			}
			if err := res.Config.Validate(); err != nil {
				return err
			}
			fmt.Fprintln(out, "configuration is valid")
			return nil
		},
	}
}

func newConfigShowCommand(opts *options) *cobra.Command {
	var showAll bool
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print the effective configuration and where each value came from",
		Long: "Prints every configuration knob with its effective value and the source that\n" +
			"supplied it: a built-in default, the configuration file, or an environment\n" +
			"variable. Secrets are shown as a fingerprint, never as their value.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			return writeFields(cmd.OutOrStdout(), res, showAll)
		},
	}
	cmd.Flags().BoolVarP(&showAll, "all", "a", false,
		"include values still at their built-in default")
	return cmd
}

func writeFields(w io.Writer, res *config.Result, showAll bool) error {
	fields := append([]config.Field(nil), res.Fields...)
	sort.Slice(fields, func(i, j int) bool { return fields[i].TOMLPath < fields[j].TOMLPath })

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SETTING\tVALUE\tSOURCE")
	shown := 0
	for _, f := range fields {
		if !showAll && f.Source == config.SourceDefault {
			continue
		}
		source := string(f.Source)
		if f.Source == config.SourceEnv && f.EnvVar != "" {
			source = "env:" + f.EnvVar
		}
		value := f.Value
		if value == "" {
			value = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", f.TOMLPath, value, source)
		shown++
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if !showAll {
		fmt.Fprintf(w, "\n%d of %d settings differ from their defaults; pass --all to see everything.\n",
			shown, len(fields))
	}
	return nil
}
