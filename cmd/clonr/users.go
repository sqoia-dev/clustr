package main

// users.go — clonr admin users subcommand: user account CRUD (ADR-0007).
//
// clonr admin users list
// clonr admin users create <username> --role <role>
// clonr admin users reset-password <id|username>
// clonr admin users disable <id|username>

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// userRecord mirrors the server's userResponse wire type.
type userRecord struct {
	ID                 string  `json:"id"`
	Username           string  `json:"username"`
	Role               string  `json:"role"`
	MustChangePassword bool    `json:"must_change_password"`
	Disabled           bool    `json:"disabled"`
	CreatedAt          string  `json:"created_at"`
	LastLoginAt        *string `json:"last_login_at,omitempty"`
}

type listUsersResponse struct {
	Users []userRecord `json:"users"`
}

func init() {
	// Attach under the existing 'admin' subcommand if present, otherwise create it.
	var adminCmd *cobra.Command
	for _, sub := range rootCmd.Commands() {
		if sub.Use == "admin" {
			adminCmd = sub
			break
		}
	}
	if adminCmd == nil {
		adminCmd = &cobra.Command{
			Use:   "admin",
			Short: "Administrative operations (requires admin API key or user)",
		}
		rootCmd.AddCommand(adminCmd)
	}

	usersCmd := &cobra.Command{
		Use:   "users",
		Short: "Manage user accounts",
	}
	usersCmd.AddCommand(
		newUsersListCmd(),
		newUsersCreateCmd(),
		newUsersResetPasswordCmd(),
		newUsersDisableCmd(),
	)
	adminCmd.AddCommand(usersCmd)
}

// ─── users list ──────────────────────────────────────────────────────────────

func newUsersListCmd() *cobra.Command {
	var flagJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all user accounts",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			c := clientFromFlags()

			var resp listUsersResponse
			if err := c.GetJSON(ctx, "/api/v1/admin/users", &resp); err != nil {
				return fmt.Errorf("list users: %w", err)
			}

			if flagJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(resp.Users)
			}

			if len(resp.Users) == 0 {
				fmt.Println("No users found.")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tUSERNAME\tROLE\tDISABLED\tMUST CHANGE PW\tCREATED\tLAST LOGIN")
			for _, u := range resp.Users {
				disabled := "no"
				if u.Disabled {
					disabled = "YES"
				}
				mustChange := "no"
				if u.MustChangePassword {
					mustChange = "YES"
				}
				lastLogin := "never"
				if u.LastLoginAt != nil {
					if t, err := time.Parse(time.RFC3339, *u.LastLoginAt); err == nil {
						lastLogin = t.Local().Format("2006-01-02 15:04")
					}
				}
				created := ""
				if t, err := time.Parse(time.RFC3339, u.CreatedAt); err == nil {
					created = t.Local().Format("2006-01-02 15:04")
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					shortID(u.ID),
					u.Username,
					u.Role,
					disabled,
					mustChange,
					created,
					lastLogin,
				)
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&flagJSON, "json", false, "Output as JSON")
	return cmd
}

// ─── users create ─────────────────────────────────────────────────────────────

func newUsersCreateCmd() *cobra.Command {
	var (
		flagRole     string
		flagPassword string
	)
	cmd := &cobra.Command{
		Use:   "create <username>",
		Short: "Create a new user account",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if flagRole == "" {
				return fmt.Errorf("--role is required (admin, operator, readonly)")
			}
			if flagPassword == "" {
				return fmt.Errorf("--password is required")
			}
			if len(flagPassword) < 8 {
				return fmt.Errorf("password must be at least 8 characters")
			}

			ctx := context.Background()
			c := clientFromFlags()

			body := map[string]string{
				"username": args[0],
				"password": flagPassword,
				"role":     flagRole,
			}
			var u userRecord
			if err := c.PostJSON(ctx, "/api/v1/admin/users", body, &u); err != nil {
				return fmt.Errorf("create user: %w", err)
			}
			fmt.Printf("User created: %s (role: %s, id: %s)\n", u.Username, u.Role, shortID(u.ID))
			return nil
		},
	}
	cmd.Flags().StringVar(&flagRole, "role", "", "Role: admin, operator, or readonly (required)")
	cmd.Flags().StringVar(&flagPassword, "password", "", "Initial password (min 8 chars, required)")
	_ = cmd.MarkFlagRequired("role")
	_ = cmd.MarkFlagRequired("password")
	return cmd
}

// ─── users reset-password ────────────────────────────────────────────────────

func newUsersResetPasswordCmd() *cobra.Command {
	var flagPassword string
	cmd := &cobra.Command{
		Use:   "reset-password <id|username>",
		Short: "Admin reset: set a temporary password and force change on next login",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if flagPassword == "" {
				return fmt.Errorf("--password is required")
			}
			if len(flagPassword) < 8 {
				return fmt.Errorf("password must be at least 8 characters")
			}

			ctx := context.Background()
			c := clientFromFlags()

			id, err := resolveUserID(ctx, c, args[0])
			if err != nil {
				return err
			}

			body := map[string]string{"password": flagPassword}
			if err := c.PostJSON(ctx, "/api/v1/admin/users/"+id+"/reset-password", body, nil); err != nil {
				return fmt.Errorf("reset password: %w", err)
			}
			fmt.Printf("Password reset for user %s. They must change it on next login.\n", args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&flagPassword, "password", "", "Temporary password (min 8 chars, required)")
	_ = cmd.MarkFlagRequired("password")
	return cmd
}

// ─── users disable ────────────────────────────────────────────────────────────

func newUsersDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable <id|username>",
		Short: "Disable a user account (soft delete — preserves audit history)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			c := clientFromFlags()

			id, err := resolveUserID(ctx, c, args[0])
			if err != nil {
				return err
			}

			if err := c.DeleteJSON(ctx, "/api/v1/admin/users/"+id); err != nil {
				return fmt.Errorf("disable user: %w", err)
			}
			fmt.Printf("User %s disabled.\n", args[0])
			return nil
		},
	}
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// resolveUserID maps a username or ID to a canonical UUID.
func resolveUserID(ctx context.Context, c interface{ GetJSON(context.Context, string, interface{}) error }, nameOrID string) (string, error) {
	// If it looks like a UUID (36 chars with dashes), use it directly.
	if isUUID(nameOrID) {
		return nameOrID, nil
	}
	// Otherwise list all users and match by username or short ID prefix.
	var resp listUsersResponse
	if err := c.GetJSON(ctx, "/api/v1/admin/users", &resp); err != nil {
		return "", fmt.Errorf("resolve user: %w", err)
	}
	for _, u := range resp.Users {
		if u.Username == nameOrID || shortID(u.ID) == nameOrID || u.ID == nameOrID {
			return u.ID, nil
		}
	}
	return "", fmt.Errorf("user %q not found", nameOrID)
}

// isUUID returns true for standard 36-char UUID strings.
func isUUID(s string) bool {
	return len(s) == 36 && countRune(s, '-') == 4
}

func countRune(s string, r rune) int {
	n := 0
	for _, c := range s {
		if c == r {
			n++
		}
	}
	return n
}
