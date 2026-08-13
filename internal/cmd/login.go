package cmd

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"os/signal"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/claudecode"
	"github.com/charmbracelet/crush/internal/clipboard"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/oauth"
	"github.com/charmbracelet/crush/internal/oauth/antigravity"
	"github.com/charmbracelet/crush/internal/oauth/codex"
	"github.com/charmbracelet/crush/internal/oauth/copilot"
	"github.com/charmbracelet/crush/internal/oauth/geminicli"
	"github.com/charmbracelet/crush/internal/oauth/hyper"
	"github.com/charmbracelet/crush/internal/workspace"
	"github.com/pkg/browser"
	"github.com/spf13/cobra"
)

var loginCmd = &cobra.Command{
	Aliases: []string{"auth"},
	Use:     "login [platform]",
	Short:   "Login Crush to a platform",
	Long: `Login Crush to a specified platform.
The platform should be provided as an argument.
Available platforms are: hyper, copilot, codex, claude, gemini, antigravity.`,
	Example: `
# Authenticate with Charm Hyper
crush login

# Authenticate with a Claude Pro/Max subscription
crush login claude

# Add a second Claude subscription account alongside the first
crush login claude --account work

# Authenticate a Claude subscription without a local browser
crush login claude --manual

# Authenticate with GitHub Copilot
crush login copilot

# Authenticate with OpenAI Codex (ChatGPT subscription)
crush login codex

# Authenticate with OpenAI Codex using the device-code flow (headless)
crush login codex --device

# Authenticate with Gemini CLI (Cloud Code Assist)
crush login gemini

# Authenticate with Google Antigravity (experimental; see
# docs/antigravity-cli-oauth-findings.md)
crush login antigravity

# Authenticate with Google Antigravity using the device-code flow
crush login antigravity --device

# Force re-authentication even if already logged in
crush login -f copilot
  `,
	ValidArgs: []cobra.Completion{
		"hyper",
		"copilot",
		"github",
		"github-copilot",
		"codex",
		"claude",
		"claude-code",
		"gemini",
		"antigravity",
	},
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ws, cleanup, err := setupWorkspaceWithProgressBar(cmd)
		if err != nil {
			return err
		}
		defer cleanup()

		provider := "hyper"
		if len(args) > 0 {
			provider = args[0]
		}
		force, _ := cmd.Flags().GetBool("force")
		device, _ := cmd.Flags().GetBool("device")
		manual, _ := cmd.Flags().GetBool("manual")
		account, _ := cmd.Flags().GetString("account")
		switch provider {
		case "hyper":
			return loginHyper(ws, force)
		case "copilot", "github", "github-copilot":
			return loginCopilot(ws, force)
		case "codex", "openai-codex", "chatgpt":
			return loginCodex(ws, force, device)
		case "claude", "claude-code", "claudecode", "claude-max", "claude-pro":
			return loginClaudeCode(ws, force, manual, account)
		case "gemini", "gemini-cli", "google-gemini-cli":
			return loginGemini(ws, force)
		case "antigravity", "agy", "google-antigravity":
			return loginAntigravity(ws, force, device)
		default:
			return fmt.Errorf("unknown platform: %s", provider)
		}
	},
}

func init() {
	loginCmd.Flags().BoolP("force", "f", false, "Force re-authentication even if already logged in")
	loginCmd.Flags().Bool("device", false, "Use the device-code flow (Codex and Antigravity only; for headless environments)")
	loginCmd.Flags().Bool("manual", false, "Paste the authorization code instead of listening for a redirect (Claude only; for headless environments)")
	loginCmd.Flags().String("account", "", "Name a second (third, ...) Claude subscription account to log in alongside the first")
}

func loginHyper(ws workspace.Workspace, force bool) error {
	ctx := getLoginContext()

	if !force {
		cfg := ws.Config()
		if cfg != nil {
			if pc, ok := cfg.Providers.Get("hyper"); ok && pc.OAuthToken != nil {
				fmt.Println("You are already logged in to Hyper.")
				fmt.Println("Use --force to re-authenticate.")
				return nil
			}
		}
	}

	resp, err := hyper.InitiateDeviceAuth(ctx)
	if err != nil {
		return err
	}

	clipboard.WriteText(resp.UserCode)
	fmt.Println("The following code should be on clipboard already:")

	fmt.Println()
	lipgloss.Println(lipgloss.NewStyle().Bold(true).Render(resp.UserCode))
	fmt.Println()
	fmt.Println("Press enter to open this URL, and then paste it there:")
	fmt.Println()
	lipgloss.Println(lipgloss.NewStyle().Hyperlink(resp.VerificationURL, "id=hyper").Render(resp.VerificationURL))
	fmt.Println()
	waitEnter()
	if err := browser.OpenURL(resp.VerificationURL); err != nil {
		fmt.Println("Could not open the URL. You'll need to manually open the URL in your browser.")
	}

	fmt.Println("Exchanging authorization code...")
	refreshToken, err := hyper.PollForToken(ctx, resp.DeviceCode, resp.ExpiresIn)
	if err != nil {
		return err
	}

	fmt.Println("Exchanging refresh token for access token...")
	token, err := hyper.ExchangeToken(ctx, refreshToken)
	if err != nil {
		return err
	}

	fmt.Println("Verifying access token...")
	introspect, err := hyper.IntrospectToken(ctx, token.AccessToken)
	if err != nil {
		return fmt.Errorf("token introspection failed: %w", err)
	}
	if !introspect.Active {
		return fmt.Errorf("access token is not active")
	}

	if err := ws.SetProviderAPIKey(config.ScopeGlobal, "hyper", token); err != nil {
		return err
	}

	fmt.Println()
	fmt.Println("You're now authenticated with Hyper!")
	return nil
}

func loginCopilot(ws workspace.Workspace, force bool) error {
	loginCtx := getLoginContext()

	if !force {
		cfg := ws.Config()
		if cfg != nil {
			if pc, ok := cfg.Providers.Get("copilot"); ok && pc.OAuthToken != nil {
				fmt.Println("You are already logged in to GitHub Copilot.")
				fmt.Println("Use --force to re-authenticate.")
				return nil
			}
		}
	}

	diskToken, hasDiskToken := copilot.RefreshTokenFromDisk()
	var token *oauth.Token

	switch {
	case hasDiskToken:
		fmt.Println("Found existing GitHub Copilot token on disk. Using it to authenticate...")

		t, err := copilot.RefreshToken(loginCtx, diskToken)
		if err != nil {
			return fmt.Errorf("unable to refresh token from disk: %w", err)
		}
		token = t
	default:
		fmt.Println("Requesting device code from GitHub...")
		dc, err := copilot.RequestDeviceCode(loginCtx)
		if err != nil {
			return err
		}

		clipboard.WriteText(dc.UserCode)
		fmt.Println()
		fmt.Println("The following code should be on clipboard already:")
		fmt.Println()
		lipgloss.Println(lipgloss.NewStyle().Bold(true).Render(dc.UserCode))
		fmt.Println()
		fmt.Println("Press enter to open this URL and authenticate with GitHub Copilot:")
		fmt.Println()
		lipgloss.Println(lipgloss.NewStyle().Hyperlink(dc.VerificationURI, "id=copilot").Render(dc.VerificationURI))
		fmt.Println()
		waitEnter()
		if err := browser.OpenURL(dc.VerificationURI); err != nil {
			fmt.Println("Could not open the URL. You'll need to manually open the URL in your browser.")
		}

		fmt.Println("Waiting for authorization...")

		t, err := copilot.PollForToken(loginCtx, dc)
		if err == copilot.ErrNotAvailable {
			fmt.Println()
			fmt.Println("GitHub Copilot is unavailable for this account. To signup, go to the following page:")
			fmt.Println()
			lipgloss.Println(lipgloss.NewStyle().Hyperlink(copilot.SignupURL, "id=copilot-signup").Render(copilot.SignupURL))
			fmt.Println()
			fmt.Println("You may be able to request free access if eligible. For more information, see:")
			fmt.Println()
			lipgloss.Println(lipgloss.NewStyle().Hyperlink(copilot.FreeURL, "id=copilot-free").Render(copilot.FreeURL))
		}
		if err != nil {
			return err
		}
		token = t
	}

	if err := ws.SetProviderAPIKey(config.ScopeGlobal, "copilot", token); err != nil {
		return err
	}

	fmt.Println()
	fmt.Println("You're now authenticated with GitHub Copilot!")
	return nil
}

func loginCodex(ws workspace.Workspace, force, device bool) error {
	ctx := getLoginContext()

	if !force {
		cfg := ws.Config()
		if cfg != nil {
			if pc, ok := cfg.Providers.Get(codex.ProviderID); ok && pc.OAuthToken != nil {
				fmt.Println("You are already logged in to OpenAI Codex.")
				fmt.Println("Use --force to re-authenticate.")
				return nil
			}
		}
	}

	var (
		token *oauth.Token
		err   error
	)
	if device {
		token, err = codex.LoginDevice(ctx)
	} else {
		token, err = codex.LoginBrowser(ctx)
	}
	if err != nil {
		return err
	}

	if err := cmp.Or(
		ws.SetConfigField(config.ScopeGlobal, "providers."+codex.ProviderID+".api_key", token.AccessToken),
		ws.SetConfigField(config.ScopeGlobal, "providers."+codex.ProviderID+".oauth", token),
	); err != nil {
		return err
	}

	fmt.Println()
	fmt.Println("You're now authenticated with OpenAI Codex!")
	return nil
}

// loginClaudeCode authenticates a Claude Pro/Max subscription. Each
// account lives in its own provider entry — the default account under
// "claude-code" and every named one under "claude-code-<account>" — so
// several subscriptions can be configured at once and switched between by
// picking the matching provider in the model list.
func loginClaudeCode(ws workspace.Workspace, force, manual bool, account string) error {
	ctx := getLoginContext()

	if account != "" && claudecode.AccountSlug(account) == "" {
		return fmt.Errorf("account name %q has no letters or digits to name a provider with", account)
	}
	providerID := claudecode.ProviderIDForAccount(account)

	cfg := ws.Config()
	if !force && cfg != nil {
		if pc, ok := cfg.Providers.Get(providerID); ok && pc.OAuthToken != nil {
			fmt.Printf("You are already logged in to Claude as %s.\n", claudeAccountLabel(providerID, pc.OAuthExtra))
			fmt.Println("Use --force to re-authenticate, or --account <name> to add another account.")
			return nil
		}
	}

	var (
		token *oauth.Token
		acct  claudecode.Account
		err   error
	)
	if manual {
		token, acct, err = claudecode.LoginManual(ctx)
	} else {
		token, acct, err = claudecode.LoginBrowser(ctx)
	}
	if err != nil {
		return err
	}

	// Adding the same subscription twice under two names would silently
	// share one rate limit, which defeats the point of a second account.
	if cfg != nil && acct.UUID != "" {
		for id, pc := range cfg.Providers.Seq2() {
			if id == providerID || !claudecode.IsProviderID(id) || pc.OAuthExtra == nil {
				continue
			}
			if pc.OAuthExtra["account_uuid"] == acct.UUID {
				fmt.Printf("\nNote: this is the same Anthropic account already stored as %q;\n", id)
				fmt.Println("both providers will draw on the same subscription limits.")
			}
		}
	}

	extra := map[string]string{}
	if acct.UUID != "" {
		extra["account_uuid"] = acct.UUID
	}
	if acct.Email != "" {
		extra["email"] = acct.Email
	}
	if acct.OrganizationUUID != "" {
		extra["organization_uuid"] = acct.OrganizationUUID
	}

	if err := cmp.Or(
		ws.SetConfigField(config.ScopeGlobal, "providers."+providerID+".oauth", token),
		ws.SetConfigField(config.ScopeGlobal, "providers."+providerID+".oauth_extra", extra),
	); err != nil {
		return err
	}

	fmt.Println()
	fmt.Printf("You're now authenticated with Claude as %s!\n", claudeAccountLabel(providerID, extra))
	if account != "" {
		fmt.Printf("This account is available as the %q provider — select it in the model list to use it.\n", providerID)
	}
	return nil
}

// claudeAccountLabel describes a stored subscription account for CLI
// output, preferring the email recorded at login over the provider id.
func claudeAccountLabel(providerID string, extra map[string]string) string {
	if extra != nil && extra["email"] != "" {
		return extra["email"]
	}
	if account := claudecode.AccountFromProviderID(providerID); account != "" {
		return account
	}
	return providerID
}

func loginGemini(ws workspace.Workspace, force bool) error {
	ctx := getLoginContext()

	if !force {
		cfg := ws.Config()
		if cfg != nil {
			if pc, ok := cfg.Providers.Get(geminicli.ProviderID); ok && pc.OAuthToken != nil {
				fmt.Println("You are already logged in to Gemini CLI.")
				fmt.Println("Use --force to re-authenticate.")
				return nil
			}
		}
	}

	token, projectID, email, err := geminicli.LoginBrowser(ctx)
	if err != nil {
		return err
	}

	extra := map[string]string{"project_id": projectID}
	if email != "" {
		extra["email"] = email
	}

	if err := cmp.Or(
		ws.SetConfigField(config.ScopeGlobal, "providers."+geminicli.ProviderID+".api_key", token.AccessToken),
		ws.SetConfigField(config.ScopeGlobal, "providers."+geminicli.ProviderID+".oauth", token),
		ws.SetConfigField(config.ScopeGlobal, "providers."+geminicli.ProviderID+".oauth_extra", extra),
	); err != nil {
		return err
	}

	fmt.Println()
	fmt.Println("You're now authenticated with Gemini CLI!")
	return nil
}

func loginAntigravity(ws workspace.Workspace, force, device bool) error {
	ctx := getLoginContext()

	if !force {
		cfg := ws.Config()
		if cfg != nil {
			if pc, ok := cfg.Providers.Get(antigravity.ProviderID); ok && pc.OAuthToken != nil {
				fmt.Println("You are already logged in to Google Antigravity.")
				fmt.Println("Use --force to re-authenticate.")
				return nil
			}
		}
	}

	fmt.Println("Note: Antigravity login is experimental and based on reverse-engineered")
	fmt.Println("OAuth parameters (see docs/antigravity-cli-oauth-findings.md). If project")
	fmt.Println("discovery fails with a tier/Cloud project error, that's a known open issue.")
	fmt.Println()

	var (
		token *oauth.Token
		err   error
	)
	if device {
		token, err = antigravity.LoginDevice(ctx)
	} else {
		token, err = antigravity.LoginBrowser(ctx)
	}
	if err != nil {
		return err
	}

	fmt.Println("Discovering Cloud Code Assist project...")
	projectID, err := geminicli.DiscoverProject(ctx, token.AccessToken, antigravity.Identity)
	if err != nil {
		return err
	}
	extra := map[string]string{"project_id": projectID}

	if err := cmp.Or(
		ws.SetConfigField(config.ScopeGlobal, "providers."+antigravity.ProviderID+".api_key", token.AccessToken),
		ws.SetConfigField(config.ScopeGlobal, "providers."+antigravity.ProviderID+".oauth", token),
		ws.SetConfigField(config.ScopeGlobal, "providers."+antigravity.ProviderID+".oauth_extra", extra),
	); err != nil {
		return err
	}

	fmt.Println()
	fmt.Println("You're now authenticated with Google Antigravity!")
	return nil
}

func getLoginContext() context.Context {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill)
	go func() {
		<-ctx.Done()
		cancel()
		os.Exit(1)
	}()
	return ctx
}

func waitEnter() {
	_, _ = fmt.Scanln()
}
