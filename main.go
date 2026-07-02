// Package main is the entry point for the Crush CLI.
//
//	@title			Crush API
//	@version		1.0
//	@description	Crush is a terminal-based AI coding assistant. This API is served over a Unix socket (or Windows named pipe) and provides programmatic access to workspaces, sessions, agents, LSP, MCP, and more.
//	@contact.name	Charm
//	@contact.url	https://charm.sh
//	@license.name	MIT
//	@license.url	https://github.com/charmbracelet/crush/blob/main/LICENSE
//	@BasePath		/v1
package main

import (
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"os"

	"github.com/charmbracelet/crush/internal/cmd"
	_ "github.com/charmbracelet/crush/internal/dns"
	_ "github.com/joho/godotenv/autoload"
)

func main() {
	if profile := os.Getenv("CRUSH_PROFILE"); profile != "" {
		// Allow overriding the pprof listen address (default :6060 often
		// collides with other tools like the Coder agent). Any value
		// other than a bare "1"/"true" is treated as the listen address.
		addr := "localhost:6060"
		switch profile {
		case "1", "true", "TRUE", "on":
		default:
			addr = profile
		}
		go func() {
			slog.Info("Serving pprof", "addr", addr)
			if httpErr := http.ListenAndServe(addr, nil); httpErr != nil {
				slog.Error("Failed to pprof listen", "error", httpErr)
			}
		}()
	}

	cmd.Execute()
}
