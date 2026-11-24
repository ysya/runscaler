package provider

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"

	"github.com/distribution/reference"
	"github.com/moby/moby/api/pkg/authconfig"
	"github.com/moby/moby/api/types/registry"
	dockerclient "github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
)

// PullDockerImage uses the manager's Docker login for the registry of this image.
// Look it up at pull time so a running manager picks up renewed credentials.
func PullDockerImage(ctx context.Context, client DockerAPI, image string, platform *ocispec.Platform) (dockerclient.ImagePullResponse, error) {
	ref, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return nil, fmt.Errorf("invalid Docker image reference: %w", err)
	}
	header, err := dockerRegistryAuth(ctx, reference.Domain(ref))
	if err != nil {
		return nil, err
	}
	opts := dockerclient.ImagePullOptions{RegistryAuth: header}
	if platform != nil {
		opts.Platforms = []ocispec.Platform{*platform}
	}
	return client.ImagePull(ctx, image, opts)
}

func dockerRegistryAuth(ctx context.Context, domain string) (string, error) {
	dir := dockerConfigDir()
	if dir == "" {
		return "", nil
	}
	store, err := credentials.NewStore(filepath.Join(dir, "config.json"), credentials.StoreOptions{DetectDefaultNativeStore: true})
	if err != nil {
		return "", fmt.Errorf("load Docker config for image pull: %w", err)
	}
	cred, err := store.Get(ctx, credentials.ServerAddressFromRegistry(domain))
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// Helper errors may contain the helper's output, including secrets.
		return "", fmt.Errorf("cannot read Docker credentials for %s: check the service account's Docker config and credential helper", domain)
	}
	if cred == auth.EmptyCredential {
		return "", nil
	}
	return authconfig.Encode(registry.AuthConfig{
		ServerAddress: domain,
		Username:      cred.Username,
		Password:      cred.Password,
		IdentityToken: cred.RefreshToken,
		RegistryToken: cred.AccessToken,
	})
}

func dockerConfigDir() string {
	if dir := os.Getenv("DOCKER_CONFIG"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// A system service can have an account home without a HOME variable.
		account, err := user.Current()
		if err != nil || account.HomeDir == "" {
			return ""
		}
		home = account.HomeDir
	}
	return filepath.Join(home, ".docker")
}
