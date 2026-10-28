package main

import (
	"fmt"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/ysya/runscaler/internal/config"
)

func registerGitHubAppFlags(flags *pflag.FlagSet) {
	flags.String("github-app-client-id", "", "GitHub App client ID (or numeric App ID)")
	flags.Int64("github-app-installation-id", 0, "GitHub App installation ID")
	flags.String("github-app-private-key-file", "", "Absolute path to the GitHub App RSA private key file")
	flags.String("github-app-private-key-env", "", "Name of the environment variable containing the GitHub App RSA private key")
}

func bindGitHubAppFlags(v *viper.Viper, flags *pflag.FlagSet) {
	for _, key := range []string{"client-id", "installation-id", "private-key-file", "private-key-env"} {
		if flag := flags.Lookup("github-app-" + key); flag != nil && flag.Changed {
			_ = v.BindPFlag("github-app."+key, flag)
		}
	}
}

func githubAppFromFlags(flags *pflag.FlagSet) *config.GitHubAppConfig {
	selected := false
	for _, key := range []string{"client-id", "installation-id", "private-key-file", "private-key-env"} {
		if flag := flags.Lookup("github-app-" + key); flag != nil && flag.Changed {
			selected = true
		}
	}
	if !selected {
		return nil
	}
	clientID, _ := flags.GetString("github-app-client-id")
	installationID, _ := flags.GetInt64("github-app-installation-id")
	keyFile, _ := flags.GetString("github-app-private-key-file")
	keyEnv, _ := flags.GetString("github-app-private-key-env")
	return &config.GitHubAppConfig{ClientID: clientID, InstallationID: installationID, PrivateKeyFile: keyFile, PrivateKeyEnv: keyEnv}
}

func initAuthenticationConfig(token string, app *config.GitHubAppConfig) string {
	if app == nil {
		return fmt.Sprintf("# Personal access token (consider using env:VARIABLE_NAME for security)\n# Example: token = \"env:GITHUB_TOKEN\"\ntoken = %q", token)
	}
	result := fmt.Sprintf("# GitHub App credential references\ngithub-app.client-id = %q\ngithub-app.installation-id = %d\n", app.ClientID, app.InstallationID)
	if app.PrivateKeyFile != "" {
		return result + fmt.Sprintf("github-app.private-key-file = %q", app.PrivateKeyFile)
	}
	return result + fmt.Sprintf("github-app.private-key-env = %q", app.PrivateKeyEnv)
}
