package cmd

import (
	"github.com/spf13/cobra"
	genFigSpec "github.com/withfig/autocomplete-tools/packages/cobra"
)

func newFigGenCommand() *Command {
	figCmd := genFigSpec.NewCmdGenFigSpec()

	var figGenCmd = &cobra.Command{
		Use:     "generateFigSpec",
		Aliases: []string{"genFigSpec"},
		Short:   "Generate a fig spec",
		Hidden:  true,
		Long: `
	Fig is a tool for your command line that adds autocomplete.
	This command generates a TypeScript file with the skeleton
	Fig autocomplete spec for your Cobra CLI.
	`,
		RunE: func(cmd *cobra.Command, args []string) error {
			figCmd.Run(cmd, args)

			return nil
		},
	}

	c := &Command{
		Command: figGenCmd,
	}

	return c
}
