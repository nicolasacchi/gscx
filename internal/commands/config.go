package commands

import (
	"fmt"
	"os"

	"github.com/nicolasacchi/gscx/internal/config"
	"github.com/spf13/cobra"
)

var (
	flagConfigReveal bool
	flagConfigForce  bool
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Show or initialize the configuration",
}

var configPathCmd = &cobra.Command{
	Use:   "path",
	Short: "Print the config file path",
	RunE: func(cmd *cobra.Command, args []string) error {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), configPath())
		return err
	},
}

var configShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Print the effective configuration as TOML (secrets masked)",
	RunE: func(cmd *cobra.Command, args []string) error {
		b, err := config.Encode(cfg, flagConfigReveal)
		if err != nil {
			return err
		}
		_, err = cmd.OutOrStdout().Write(b)
		return err
	},
}

var configInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Write the built-in defaults to the config file",
	Long: `Writes the built-in defaults (site, sitemaps, inspection budget, Datadog
queries, page-type rules) to the config file so they can be edited.
Refuses to overwrite an existing file unless --force is given.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		path := configPath()
		if _, err := os.Stat(path); err == nil && !flagConfigForce {
			return fmt.Errorf("%s exists; pass --force to overwrite", path)
		}
		if err := config.Save(path, config.Defaults()); err != nil {
			return err
		}
		return emit(cmd, map[string]any{"written": path})
	},
}

func configPath() string {
	if flagConfig != "" {
		return flagConfig
	}
	return config.Path()
}

func init() {
	configShowCmd.Flags().BoolVar(&flagConfigReveal, "reveal", false, "print secrets unmasked")
	configInitCmd.Flags().BoolVar(&flagConfigForce, "force", false, "overwrite an existing config file")
	configCmd.AddCommand(configPathCmd, configShowCmd, configInitCmd)
	rootCmd.AddCommand(configCmd)
}
