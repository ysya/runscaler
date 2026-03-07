package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Generate a config file interactively",
	Long:  "Create a config.toml file by answering a few questions. Flags can be used for non-interactive mode.",
	Example: `  # Interactive mode
  runscaler init

  # Non-interactive mode
  runscaler init --url https://github.com/org --name my-runners --token ghp_xxx`,
	RunE: runInit,
}

func init() {
	flags := initCmd.Flags()
	flags.String("url", "", "Registration URL (e.g. https://github.com/org)")
	flags.String("name", "", "Scale set name")
	flags.String("token", "", "Personal access token")
	flags.Int("max-runners", 10, "Maximum concurrent runners")
	flags.Bool("dind", true, "Enable Docker-in-Docker")
	flags.String("output", "config.toml", "Output file path")
}

func runInit(cmd *cobra.Command, args []string) error {
	output, _ := cmd.Flags().GetString("output")

	// Check if file exists
	if _, err := os.Stat(output); err == nil {
		overwrite, err := promptYN(fmt.Sprintf("%s already exists. Overwrite?", output), false)
		if err != nil {
			return err
		}
		if !overwrite {
			fmt.Println("Cancelled.")
			return nil
		}
	}

	url, _ := cmd.Flags().GetString("url")
	name, _ := cmd.Flags().GetString("name")
	token, _ := cmd.Flags().GetString("token")
	maxRunners, _ := cmd.Flags().GetInt("max-runners")
	dind, _ := cmd.Flags().GetBool("dind")

	// Interactive mode: prompt for missing values
	var err error
	if url == "" {
		url, err = promptString("GitHub registration URL (e.g. https://github.com/your-org)")
		if err != nil {
			return err
		}
	}
	if name == "" {
		name, err = promptString("Scale set name (used as runs-on label)")
		if err != nil {
			return err
		}
	}
	if token == "" {
		token, err = promptSecret("GitHub Personal Access Token")
		if err != nil {
			return err
		}
	}
	if !cmd.Flags().Changed("max-runners") {
		maxRunners, err = promptInt("Maximum concurrent runners", 10)
		if err != nil {
			return err
		}
	}
	if !cmd.Flags().Changed("dind") {
		dind, err = promptYN("Enable Docker-in-Docker?", true)
		if err != nil {
			return err
		}
	}

	config := fmt.Sprintf(`# runscaler configuration
# See: https://github.com/ysya/runscaler

# GitHub registration URL (organization or repository)
url = %q

# Scale set name — used as the runs-on label in workflows
name = %q

# Personal access token (consider using env:VARIABLE_NAME for security)
# Example: token = "env:GITHUB_TOKEN"
token = %q

# Runner limits
max-runners = %d
min-runners = 0

# Docker image for runners
runner-image = "ghcr.io/actions/actions-runner:latest"

# Docker-in-Docker: mount host Docker socket into runners
dind = %v

# Docker socket path
docker-socket = "/var/run/docker.sock"

# Shared volume for cross-runner caching (optional)
# shared-volume = "/shared"

# Logging
log-level = "info"
log-format = "text"

# Health check server port (0 to disable)
# health-port = 8080

# --- Multi-org example ---
# Uncomment and duplicate [[scaleset]] blocks for multiple orgs:
#
# [[scaleset]]
# url = "https://github.com/org-a"
# name = "runners-a"
# token = "env:TOKEN_ORG_A"
# max-runners = 5
#
# [[scaleset]]
# url = "https://github.com/org-b"
# name = "runners-b"
# token = "env:TOKEN_ORG_B"
`, url, name, token, maxRunners, dind)

	if err := os.WriteFile(output, []byte(config), 0600); err != nil {
		return fmt.Errorf("failed to write %s: %w", output, err)
	}

	fmt.Printf("\nCreated %s\n", output)
	fmt.Println("\nNext steps:")
	fmt.Printf("  runscaler validate --config %s   # Verify configuration\n", output)
	fmt.Printf("  runscaler --config %s            # Start scaling\n", output)
	return nil
}

var reader = bufio.NewReader(os.Stdin)

func promptString(label string) (string, error) {
	for {
		fmt.Printf("%s: ", label)
		input, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		input = strings.TrimSpace(input)
		if input != "" {
			return input, nil
		}
	}
}

func promptSecret(label string) (string, error) {
	fmt.Printf("%s: ", label)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println() // newline after hidden input
	if err != nil {
		// Fall back to regular input if terminal is not available
		return promptString(label)
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return promptSecret(label)
	}
	return s, nil
}

func promptInt(label string, defaultVal int) (int, error) {
	fmt.Printf("%s [%d]: ", label, defaultVal)
	input, err := reader.ReadString('\n')
	if err != nil {
		return 0, err
	}
	input = strings.TrimSpace(input)
	if input == "" {
		return defaultVal, nil
	}
	v, err := strconv.Atoi(input)
	if err != nil {
		return 0, fmt.Errorf("invalid number: %s", input)
	}
	return v, nil
}

func promptYN(label string, defaultVal bool) (bool, error) {
	defStr := "Y/n"
	if !defaultVal {
		defStr = "y/N"
	}
	fmt.Printf("%s [%s]: ", label, defStr)
	input, err := reader.ReadString('\n')
	if err != nil {
		return false, err
	}
	input = strings.TrimSpace(strings.ToLower(input))
	switch input {
	case "":
		return defaultVal, nil
	case "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	default:
		return false, fmt.Errorf("invalid input: %s", input)
	}
}
