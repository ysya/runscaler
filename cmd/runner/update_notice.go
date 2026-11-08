package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/url"
)

const upgradeRequirementsURL = "https://github.com/ysya/runscaler/blob/main/README.md#upgrade-requirements"

func releaseNotesURL(tag string) string {
	return "https://github.com/ysya/runscaler/releases/tag/" + url.PathEscape(tag)
}

func printUpgradeNotice(out io.Writer, tag string) {
	fmt.Fprintf(out, "Release notes: %s\nUpgrade requirements: %s\n", releaseNotesURL(tag), upgradeRequirementsURL)
	fmt.Fprintln(out, "This command verifies the download checksum; it does not validate configuration/provider compatibility, rewrite service definitions or restart services. Existing service scope and runtime user are retained.")
}

func logAvailableUpdate(logger *slog.Logger, current, latest string) {
	logger.Warn("A newer version of runner is available; review the upgrade requirements before running 'runner update'. Updating does not validate compatibility or restart services",
		slog.String("current", current), slog.String("latest", latest),
		slog.String("release_notes", releaseNotesURL(latest)),
		slog.String("upgrade_requirements", upgradeRequirementsURL))
}
