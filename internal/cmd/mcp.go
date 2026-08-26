package cmd

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/config"
)

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Manage MCP servers",
	Long:  `Manage MCP (Model Context Protocol) servers configured in crush.json.`,
}

var mcpLoginCmd = &cobra.Command{
	Use:   "login <server>",
	Short: "Authorize an MCP server with OAuth",
	Long: `Run the OAuth authorization flow for an HTTP MCP server and store the
resulting token. The server must have an "oauth" block in its configuration
with at least a client_id.`,
	Example: `# Authorize a configured MCP server
crush mcp login gmail`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := initMCPConfig(cmd)
		if err != nil {
			return err
		}
		noBrowser, _ := cmd.Flags().GetBool("no-browser")
		return mcp.Login(cmd.Context(), cfg, args[0], noBrowser)
	},
}

var mcpLogoutCmd = &cobra.Command{
	Use:   "logout <server>",
	Short: "Remove a stored MCP OAuth token",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := initMCPConfig(cmd)
		if err != nil {
			return err
		}
		return mcp.Logout(cfg, args[0])
	},
}

var mcpListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List configured MCP servers",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := initMCPConfig(cmd)
		if err != nil {
			return err
		}
		servers := cfg.Config().MCP
		if len(servers) == 0 {
			fmt.Println("No MCP servers configured.")
			return nil
		}
		names := make([]string, 0, len(servers))
		for name := range servers {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			m := servers[name]
			var notes []string
			if m.Disabled {
				notes = append(notes, "disabled")
			}
			if m.OAuth != nil {
				if m.OAuth.Token != nil && m.OAuth.Token.AccessToken != "" {
					notes = append(notes, "authorized")
				} else {
					notes = append(notes, "needs login")
				}
			}
			line := fmt.Sprintf("%s\t%s", name, m.Type)
			if len(notes) > 0 {
				line += "\t(" + strings.Join(notes, ", ") + ")"
			}
			fmt.Println(line)
		}
		return nil
	},
}

func initMCPConfig(cmd *cobra.Command) (*config.ConfigStore, error) {
	cwd, err := ResolveCwd(cmd)
	if err != nil {
		return nil, err
	}
	dataDir, _ := cmd.Flags().GetString("data-dir")
	debug, _ := cmd.Flags().GetBool("debug")
	return config.Init(cwd, dataDir, debug)
}

func init() {
	mcpLoginCmd.Flags().Bool("no-browser", false, "Print the authorization URL and read the redirected URL from stdin instead of opening a browser")
	mcpCmd.AddCommand(mcpLoginCmd, mcpLogoutCmd, mcpListCmd)
}
