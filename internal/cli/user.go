package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/esaiaswestberg/imapped/internal/app"
	"github.com/esaiaswestberg/imapped/internal/config"
	"github.com/esaiaswestberg/imapped/internal/crypto"
	"github.com/esaiaswestberg/imapped/internal/logging"
	"github.com/esaiaswestberg/imapped/internal/store"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func newUserCommand(opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "user",
		Short: "Manage the people who can sign in",
		Long: "Creating the first user is the only step that needs a shell; everything\n" +
			"else is done through the web interface.",
	}
	cmd.AddCommand(newUserCreateCommand(opts), newUserPasswordCommand(opts))
	return cmd
}

func newUserCreateCommand(opts *options) *cobra.Command {
	var (
		email         string
		password      string
		passwordStdin bool
		admin         bool
	)

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a user",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			secret, err := readPassword(password, passwordStdin, "Password: ")
			if err != nil {
				return err
			}
			return withApp(cmd.Context(), opts, func(ctx context.Context, a *app.App) error {
				hash, err := crypto.HashPassword(secret)
				if err != nil {
					return err
				}
				user, err := a.Store.CreateUser(ctx, email, hash, admin)
				if err != nil {
					return err
				}
				role := "user"
				if user.IsAdmin {
					role = "administrator"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "created %s %s\n", role, user.Email)
				return nil
			})
		},
	}

	cmd.Flags().StringVar(&email, "email", "", "the user's email address (required)")
	cmd.Flags().StringVar(&password, "password", "",
		"the password; prefer --password-stdin so it does not appear in shell history")
	cmd.Flags().BoolVar(&passwordStdin, "password-stdin", false, "read the password from standard input")
	cmd.Flags().BoolVar(&admin, "admin", true, "grant administrator rights")
	_ = cmd.MarkFlagRequired("email")
	return cmd
}

func newUserPasswordCommand(opts *options) *cobra.Command {
	var (
		email         string
		password      string
		passwordStdin bool
	)

	cmd := &cobra.Command{
		Use:   "set-password",
		Short: "Change a user's password",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			secret, err := readPassword(password, passwordStdin, "New password: ")
			if err != nil {
				return err
			}
			return withApp(cmd.Context(), opts, func(ctx context.Context, a *app.App) error {
				user, err := a.Store.UserByEmail(ctx, email)
				if err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return fmt.Errorf("no user with the address %s", email)
					}
					return err
				}
				hash, err := crypto.HashPassword(secret)
				if err != nil {
					return err
				}
				if err := a.Store.SetUserPassword(ctx, user.ID, hash); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "password updated for %s\n", user.Email)
				return nil
			})
		},
	}

	cmd.Flags().StringVar(&email, "email", "", "the user's email address (required)")
	cmd.Flags().StringVar(&password, "password", "", "the new password")
	cmd.Flags().BoolVar(&passwordStdin, "password-stdin", false, "read the password from standard input")
	_ = cmd.MarkFlagRequired("email")
	return cmd
}

// readPassword resolves a password from a flag, standard input, or a prompt.
//
// Prompting is preferred where possible: a password passed as a flag is visible
// in the process list and shell history.
func readPassword(flagValue string, fromStdin bool, prompt string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if fromStdin {
		reader := bufio.NewReader(os.Stdin)
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("reading the password from standard input: %w", err)
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", errors.New("no password given: pass --password-stdin or run this on a terminal")
	}

	fmt.Fprint(os.Stderr, prompt)
	secret, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("reading the password: %w", err)
	}
	if len(secret) == 0 {
		return "", errors.New("the password must not be empty")
	}
	return string(secret), nil
}

// withApp opens the application, runs fn, and closes it.
func withApp(ctx context.Context, opts *options, fn func(context.Context, *app.App) error) error {
	res, err := config.MustLoad(opts.configPath)
	if err != nil {
		return err
	}
	log := logging.New(res.Config)

	application, err := app.Open(ctx, res.Config, log)
	if err != nil {
		return err
	}
	defer application.Close()

	return fn(ctx, application)
}
