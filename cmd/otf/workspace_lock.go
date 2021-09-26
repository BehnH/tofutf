package main

import (
	"fmt"

	"github.com/leg100/otf"
	"github.com/leg100/otf/http"
	"github.com/spf13/cobra"
)

func WorkspaceLockCommand(factory http.ClientFactory) *cobra.Command {
	var specifier otf.WorkspaceSpecifier

	cmd := &cobra.Command{
		Use:   "lock [name]",
		Short: "Lock a workspace",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			specifier.Name = otf.String(args[0])

			client, err := factory.NewClient()
			if err != nil {
				return err
			}

			ws, err := client.Workspaces().Get(cmd.Context(), specifier)
			if err != nil {
				return err
			}

			ws, err = client.Workspaces().Lock(cmd.Context(), ws.ID, otf.WorkspaceLockOptions{})
			if err != nil {
				return err
			}

			fmt.Printf("Successfully locked workspace %s\n", ws.Name)

			return nil
		},
	}

	specifier.OrganizationName = cmd.Flags().String("organization", "", "Organization workspace belongs to")
	cmd.MarkFlagRequired("organization")

	return cmd
}
