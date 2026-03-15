package versioncheck

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// DownloadURL returns the archive download URL for a given version/os/arch.
func DownloadURL(version, goos, goarch string) string {
	return fmt.Sprintf(
		"https://github.com/%s/releases/download/%s/runscaler-%s-%s.tar.gz",
		githubRepo, version, goos, goarch,
	)
}

// ChecksumURL returns the checksums.txt URL for a given version.
func ChecksumURL(version string) string {
	return fmt.Sprintf(
		"https://github.com/%s/releases/download/%s/checksums.txt",
		githubRepo, version,
	)
}

// Update downloads the given release version, verifies its checksum, and
// atomically replaces the running binary. destPath is the path to replace
// (usually the result of os.Executable()).
func Update(ctx context.Context, version string, destPath string) error {
	goos := runtime.GOOS
	goarch := runtime.GOARCH

	archiveName := fmt.Sprintf("runscaler-%s-%s.tar.gz", goos, goarch)
	archiveURL := DownloadURL(version, goos, goarch)
	checksumURL := ChecksumURL(version)

	tmpDir, err := os.MkdirTemp("", "runscaler-update-*")
	if err != nil {
		return fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Download archive
	archivePath := filepath.Join(tmpDir, archiveName)
	if err := downloadFile(ctx, archivePath, archiveURL); err != nil {
		return fmt.Errorf("failed to download %s: %w", archiveURL, err)
	}

	// Download checksums
	checksumsPath := filepath.Join(tmpDir, "checksums.txt")
	if err := downloadFile(ctx, checksumsPath, checksumURL); err != nil {
		return fmt.Errorf("failed to download checksums: %w", err)
	}

	// Verify checksum
	expected, err := expectedChecksum(checksumsPath, archiveName)
	if err != nil {
		return err
	}
	actual, err := sha256File(archivePath)
	if err != nil {
		return err
	}
	if expected != actual {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expected, actual)
	}

	// Extract binary from archive
	binaryPath := filepath.Join(tmpDir, "runscaler")
	if err := extractBinary(archivePath, "runscaler", binaryPath); err != nil {
		return fmt.Errorf("failed to extract binary: %w", err)
	}

	// Atomically replace the running binary
	// Write to a temp file in the same directory (same filesystem) so Rename is atomic.
	destDir := filepath.Dir(destPath)
	tmp, err := os.CreateTemp(destDir, ".runscaler-update-*")
	if err != nil {
		return fmt.Errorf("failed to create temp file in %s: %w", destDir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // clean up if rename fails

	src, err := os.Open(binaryPath)
	if err != nil {
		tmp.Close()
		return err
	}
	defer src.Close()

	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to write new binary: %w", err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to chmod new binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if err := os.Rename(tmpName, destPath); err != nil {
		return fmt.Errorf("failed to replace binary: %w", err)
	}

	return nil
}

func downloadFile(ctx context.Context, dest, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, resp.Body)
	return err
}

func expectedChecksum(checksumsPath, archiveName string) (string, error) {
	data, err := os.ReadFile(checksumsPath)
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == archiveName {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("checksum not found for %s", archiveName)
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func extractBinary(archivePath, binaryName, destPath string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if filepath.Base(hdr.Name) != binaryName {
			continue
		}

		out, err := os.Create(destPath)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, tr)
		out.Close()
		return copyErr
	}
	return fmt.Errorf("binary %q not found in archive", binaryName)
}
