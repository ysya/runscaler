package main

import (
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
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
