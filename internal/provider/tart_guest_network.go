package provider

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
)

// guestNetwork contains only the commands run inside a guest.
type guestNetwork interface {
	defaultDevice(context.Context, *TartProvider, string) (string, bool)
	setMTU(context.Context, *TartProvider, string, string, int) error
	setDNS(context.Context, *TartProvider, string, string, []string)
}

type macOSGuestNetwork struct{}
type linuxGuestNetwork struct{}

func guestNetworkFor(guest TartGuest) guestNetwork {
	if guest == TartGuestLinux {
		return linuxGuestNetwork{}
	}
	return macOSGuestNetwork{}
}

func (macOSGuestNetwork) defaultDevice(ctx context.Context, p *TartProvider, vm string) (string, bool) {
	if out, err := p.cmd.Run(ctx, "tart", "exec", vm, "/sbin/route", "-n", "get", "default"); err == nil {
		if route, err := parseRouteGet(out); err == nil {
			return route.Interface, true
		}
	}
	return "en0", true
}

func (macOSGuestNetwork) setMTU(ctx context.Context, p *TartProvider, vm, device string, mtu int) error {
	_, err := p.cmd.Run(ctx, "tart", "exec", vm, "/usr/bin/sudo", "-n", "/sbin/ifconfig", device, "mtu", strconv.Itoa(mtu))
	return err
}

func (macOSGuestNetwork) setDNS(ctx context.Context, p *TartProvider, vmName, device string, servers []string) {
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
	args := append([]string{"exec", vmName, "/usr/bin/sudo", "-n", "/usr/sbin/networksetup", "-setdnsservers", service}, servers...)
	if _, err := p.cmd.Run(ctx, "tart", args...); err != nil {
		p.logger.Warn("Failed to set the VM's DNS servers",
			slog.String("vm", vmName), slog.Any("error", err))
	}
}

func (linuxGuestNetwork) defaultDevice(ctx context.Context, p *TartProvider, vm string) (string, bool) {
	out, err := p.cmd.Run(ctx, "tart", "exec", vm, "ip", "-o", "route", "show", "default")
	if err != nil {
		return "", false
	}
	return linuxDefaultRouteDevice(out)
}

func linuxDefaultRouteDevice(out []byte) (string, bool) {
	for line := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(line)
		for i, field := range fields {
			if field == "dev" && i+1 < len(fields) {
				return fields[i+1], true
			}
		}
	}
	return "", false
}

func (linuxGuestNetwork) setMTU(ctx context.Context, p *TartProvider, vm, device string, mtu int) error {
	_, err := p.cmd.Run(ctx, "tart", "exec", vm, "sudo", "-n", "ip", "link", "set", "dev", device, "mtu", strconv.Itoa(mtu))
	return err
}

func (linuxGuestNetwork) setDNS(ctx context.Context, p *TartProvider, vm, device string, servers []string) {
	if _, err := p.cmd.Run(ctx, "tart", "exec", vm, "sh", "-c", "command -v resolvectl"); err != nil {
		p.logger.Warn("The VM has no resolvectl; keeping its DNS", slog.String("vm", vm), slog.Any("error", err))
		return
	}
	args := append([]string{"exec", vm, "sudo", "-n", "resolvectl", "dns", device}, servers...)
	if _, err := p.cmd.Run(ctx, "tart", args...); err != nil {
		p.logger.Warn("Failed to set the VM's DNS servers", slog.String("vm", vm), slog.Any("error", err))
	}
}
