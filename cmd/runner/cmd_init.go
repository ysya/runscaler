package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/ysya/runscaler/internal/config"
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Generate a config file interactively",
	Long:  "Create a config.toml file by answering a few questions. Flags can be used for non-interactive mode.",
	Example: `  # Interactive mode
  runner init

  # Non-interactive mode
  runner init --url https://github.com/org --name my-runners --token ghp_xxx`,
	RunE: runInit,
}

func init() {
	flags := initCmd.Flags()
	flags.String("url", "", "Registration URL (e.g. https://github.com/org)")
	flags.String("name", "", "Scale set name")
	flags.String("token", "", "Personal access token")
	flags.Int("max-runners", config.DefaultMaxRunners, "Maximum concurrent runners")
	flags.String("provider", "", "Instance provider (docker or tart)")
	flags.String("backend", "", "Deprecated alias for --provider")
	_ = flags.MarkDeprecated("backend", "use --provider instead")
	flags.String("runner-image", "", "Runner container or Tart VM image")
	flags.Bool("dind", config.DefaultDinD, "Enable Docker-in-Docker")
	flags.String("shared-volume", "", "Shared volume path (e.g. /shared)")
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
	providerName, _ := cmd.Flags().GetString("provider")
	legacyBackend, _ := cmd.Flags().GetString("backend")
	runnerImage, _ := cmd.Flags().GetString("runner-image")
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
		maxRunners, err = promptInt("Maximum concurrent runners", config.DefaultMaxRunners)
		if err != nil {
			return err
		}
	}

	providerChanged := cmd.Flags().Lookup("provider") != nil && cmd.Flags().Changed("provider")
	backendChanged := cmd.Flags().Lookup("backend") != nil && cmd.Flags().Changed("backend")
	if providerChanged && backendChanged {
		return fmt.Errorf("--provider and deprecated --backend cannot be used together")
	}
	if backendChanged {
		providerName = legacyBackend
	}

	// Provider selection
	if !providerChanged && !backendChanged {
		useTart, err := promptYN("Use Tart VM provider for macOS runners?", false)
		if err != nil {
			return err
		}
		if useTart {
			providerName = "tart"
		} else {
			providerName = config.DefaultProvider
		}
	}
	if providerName != "docker" && providerName != "tart" {
		return fmt.Errorf("provider must be \"docker\" or \"tart\", got %q", providerName)
	}
	if maxRunners < 1 {
		return fmt.Errorf("max-runners must be at least 1")
	}

	var configContent string
	if providerName == "tart" {
		// Tart provider config
		if runnerImage == "" || runnerImage == config.DefaultRunnerImage {
			runnerImage, err = promptString("Tart base VM image (e.g. ghcr.io/cirruslabs/macos-sequoia-xcode:latest)")
			if err != nil {
				return err
			}
		}
		if maxRunners > 2 {
			return fmt.Errorf("max-runners must be <= 2 for the Tart provider")
		}
		candidate := config.ScaleSetConfig{
			RegistrationURL: url,
			ScaleSetName:    name,
			Token:           token,
			MaxRunners:      maxRunners,
			Provider:        "tart",
			RunnerImage:     runnerImage,
			Tart:            config.TartConfig{RunnerDir: config.DefaultTartRunnerDir},
		}
		if err := candidate.Validate(); err != nil {
			return fmt.Errorf("invalid configuration: %w", err)
		}
		configContent = fmt.Sprintf(`# runner configuration
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

# --- Global ---
concurrent = %d
log-level = %q
log-format = %q
# log-file defaults to runner.log beside this config; set log-file = "" to disable

# Health check server (localhost-only by default; use 0.0.0.0 explicitly to expose)
# health-address = %q
# health-port = %d

# Instance provider: "docker" (Linux containers) or "tart" (macOS VMs)
provider = "tart"

# Runner image (Tart VM image with GitHub Actions runner pre-installed)
runner-image = %q

[tart]
# Path to the runner binary inside the VM
runner-dir = %q
`, url, name, token, maxRunners, maxRunners,
			config.DefaultLogLevel, config.DefaultLogFormat,
			config.DefaultHealthAddress, config.DefaultHealthPort,
			runnerImage, config.DefaultTartRunnerDir,
		)
	} else {
		// Docker provider config
		if runnerImage == "" {
			runnerImage = config.DefaultRunnerImage
		}
		if !cmd.Flags().Changed("dind") {
			dind, err = promptYN("Enable Docker-in-Docker?", config.DefaultDinD)
			if err != nil {
				return err
			}
		}
		sharedVolume, _ := cmd.Flags().GetString("shared-volume")
		if !cmd.Flags().Changed("shared-volume") {
			enableShared, err := promptYN("Enable shared volume for cross-job data sharing?", false)
			if err != nil {
				return err
			}
			if enableShared {
				sharedVolume = "/shared"
			}
		}
		candidate := config.ScaleSetConfig{
			RegistrationURL: url,
			ScaleSetName:    name,
			Token:           token,
			MaxRunners:      maxRunners,
			Provider:        config.DefaultProvider,
			RunnerImage:     runnerImage,
			Docker: config.DockerConfig{
				Socket:       config.DefaultDockerSocket,
				DinD:         &dind,
				SharedVolume: sharedVolume,
			},
		}
		if err := candidate.Validate(); err != nil {
			return fmt.Errorf("invalid configuration: %w", err)
		}

		configContent = fmt.Sprintf(`# runner configuration
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

# --- Global ---
concurrent = %d
log-level = %q
log-format = %q
# log-file defaults to runner.log beside this config; set log-file = "" to disable

# Health check server (localhost-only by default; use 0.0.0.0 explicitly to expose)
# health-address = %q
# health-port = %d

# Docker image for runners
runner-image = %q

# Instance provider: "docker" (Linux containers) or "tart" (macOS VMs)
provider = %q

[docker]
# Docker-in-Docker: mount host Docker socket into runners
dind = %v

# Docker socket path
socket = %q

# Shared volume for cross-job data sharing (optional)
shared-volume = %q

# Resource limits (0 = unlimited)
# memory = 8192   # MB (recommended: 6144+ for Android/Gradle builds)
# cpu = 4         # cores

# Orphaned buildx builder cleanup (off by default because it is daemon-wide).
# Enable only when the Docker daemon is dedicated to runners.
# buildx-cleanup = false
# buildx-cleanup-ttl = "24h"
# prune = false
# prune-ttl = "24h"

# --- Multi-org / mixed provider example ---
# Uncomment and duplicate [[scaleset]] blocks:
#
# [[scaleset]]
# url = "https://github.com/org-a"
# name = "linux-runners"
# token = "env:TOKEN_ORG_A"
# provider = "docker"
# max-runners = 10
#
# [[scaleset]]
# url = "https://github.com/org-a"
# name = "macos-runners"
# token = "env:TOKEN_ORG_A"
# provider = "tart"
# max-runners = 2
# runner-image = "ghcr.io/cirruslabs/macos-sequoia-xcode:latest"
`, url, name, token, maxRunners, maxRunners,
			config.DefaultLogLevel, config.DefaultLogFormat,
			config.DefaultHealthAddress, config.DefaultHealthPort,
			runnerImage, config.DefaultProvider,
			dind, config.DefaultDockerSocket, sharedVolume,
		)
	}

	if err := os.WriteFile(output, []byte(configContent), 0600); err != nil {
		return fmt.Errorf("failed to write %s: %w", output, err)
	}

	fmt.Printf("\nCreated %s\n", output)
	fmt.Println("\nNext steps:")
	fmt.Printf("  runner validate --config %s   # Verify configuration\n", output)
	fmt.Printf("  runner run --config %s        # Start scaling\n", output)
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
