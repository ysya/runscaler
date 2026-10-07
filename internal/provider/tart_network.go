package provider

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// configureGuestNetwork adapts the guest's network to the host before the
// runner starts. Failures are logged, never returned: an unadjusted guest may
// still reach GitHub, and refusing to start the runner cannot help it.
func (p *TartProvider) configureGuestNetwork(ctx context.Context, vmName string) {
	mtu, reason := p.guestMTU(ctx)
	if mtu == 0 && len(p.dns) == 0 {
		return
	}

	device := "en0"
	if out, err := p.cmd.Run(ctx, "tart", "exec", vmName, "/sbin/route", "-n", "get", "default"); err == nil {
		if route, err := parseRouteGet(out); err == nil {
			device = route.Interface
		}
	}

	if mtu > 0 {
		if _, err := p.cmd.Run(ctx, "tart", "exec", vmName,
			"/usr/bin/sudo", "-n", "/sbin/ifconfig", device, "mtu", strconv.Itoa(mtu)); err != nil {
			p.logger.Warn("Failed to set the VM's MTU; it may not reach GitHub through the host's tunnel",
				slog.String("vm", vmName), slog.Int("mtu", mtu), slog.Any("error", err))
		} else {
			p.logger.Log(ctx, p.mtuLogLevel(mtu, reason), "Set VM MTU",
				slog.String("vm", vmName), slog.Int("mtu", mtu), slog.String("reason", reason))
		}
	}
	if len(p.dns) > 0 {
		p.setGuestDNS(ctx, vmName, device)
	}
}

// mtuLogLevel is Info the first time an MTU and reason is applied and Debug
// for repeats, so a host that stays on a VPN logs it once rather than per job.
func (p *TartProvider) mtuLogLevel(mtu int, reason string) slog.Level {
	key := strconv.Itoa(mtu) + " " + reason
	p.mtuLogMu.Lock()
	defer p.mtuLogMu.Unlock()
	if key == p.lastMTULog {
		return slog.LevelDebug
	}
	p.lastMTULog = key
	return slog.LevelInfo
}

// guestMTU returns the MTU to apply to the guest, or 0 to leave it alone,
// with a reason for the log.
func (p *TartProvider) guestMTU(ctx context.Context) (int, string) {
	switch {
	case p.mtu < 0:
		return 0, ""
	case p.mtu > 0:
		return p.mtu, "configured"
	}

	host := p.githubHost
	if host == "" {
		host = "github.com"
	}
	out, err := p.cmd.Run(ctx, "/sbin/route", "-n", "get", host)
	if err == nil {
		var route routeInfo
		if route, err = parseRouteGet(out); err == nil {
			// A full-size route needs nothing, and an unreported MTU gives
			// nothing to match.
			if route.MTU <= 0 || route.MTU >= 1500 {
				return 0, ""
			}
			return route.MTU, fmt.Sprintf("host route to %s goes through %s", host, route.Interface)
		}
	}
	p.logger.Warn("Failed to look up the host's route to GitHub; leaving the VM's MTU alone",
		slog.String("host", host), slog.Any("error", err))
	return 0, ""
}

// setGuestDNS points the network service behind the guest's default
// interface at the configured resolvers.
func (p *TartProvider) setGuestDNS(ctx context.Context, vmName, device string) {
	out, err := p.cmd.Run(ctx, "tart", "exec", vmName, "/usr/sbin/networksetup", "-listnetworkserviceorder")
	if err != nil {
		p.logger.Warn("Failed to list the VM's network services; keeping its DNS",
			slog.String("vm", vmName), slog.Any("error", err))
		return
	}
	service, ok := networkServiceForDevice(out, device)
	if !ok {
		p.logger.Warn("No network service for the VM's default interface; keeping its DNS",
			slog.String("vm", vmName), slog.String("interface", device))
		return
	}
	args := append([]string{"exec", vmName, "/usr/bin/sudo", "-n", "/usr/sbin/networksetup", "-setdnsservers", service}, p.dns...)
	if _, err := p.cmd.Run(ctx, "tart", args...); err != nil {
		p.logger.Warn("Failed to set the VM's DNS servers",
			slog.String("vm", vmName), slog.Any("error", err))
	}
}

// registrationHost is the host part of a scale set URL, or "" when the URL
// does not parse.
func registrationHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// routeInfo is what `route -n get <destination>` reports for a destination.
type routeInfo struct {
	Interface string
	MTU       int // 0 when route printed no metrics table
}

// parseRouteGet extracts the outgoing interface and, when route printed its
// metrics table, the route's MTU — the value the host's own TCP derives its
// MSS from, so it is exactly what the guest has to stay under.
func parseRouteGet(out []byte) (routeInfo, error) {
	var info routeInfo
	lines := strings.Split(string(out), "\n")
	for i, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "interface:" {
			info.Interface = fields[1]
		}
		// The metrics are a header row naming each column ("... hopcount mtu
		// expire") followed by a row of values in the same column order.
		if col := slices.Index(fields, "mtu"); col >= 0 && i+1 < len(lines) {
			values := strings.Fields(lines[i+1])
			if col < len(values) {
				if mtu, err := strconv.Atoi(values[col]); err == nil {
					info.MTU = mtu
				}
			}
		}
	}
	if info.Interface == "" {
		return routeInfo{}, fmt.Errorf("no interface in route output: %s", strings.TrimSpace(string(out)))
	}
	return info, nil
}

var serviceOrderLine = regexp.MustCompile(`^\(\d+\) (.+)$`)

// networkServiceForDevice maps a BSD device such as en0 to the network
// service name networksetup expects. `networksetup -listnetworkserviceorder`
// prints each "(N) Name" line followed by "(Hardware Port: ..., Device: en0)".
func networkServiceForDevice(out []byte, device string) (string, bool) {
	var service string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if m := serviceOrderLine.FindStringSubmatch(line); m != nil {
			service = m[1]
			continue
		}
		if service != "" && strings.HasSuffix(line, "Device: "+device+")") {
			return service, true
		}
	}
	return "", false
}
