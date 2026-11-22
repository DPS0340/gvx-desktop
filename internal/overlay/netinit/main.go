//go:build linux

// Command netinit starts a container that the experimental sandbox engine
// joined to a user network, or that has extra hosts. The engine mounts it
// at guestcfg.Path and runs it in place of the container's command, as
// root. It makes a WireGuard link to the app's hub with the container's
// addresses, adds the extra hosts to /etc/hosts, takes the default route
// away from a container only on internal networks, drops to the
// container's user, and executes the container's command, which then is
// the sandbox's first process as it would be without netinit.
//
// What cannot be set up is reported on the standard error and the command
// runs anyway.
package main

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/ironpark/gvx-desktop/internal/overlay/guestcfg"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "netinit: no command")
		os.Exit(127)
	}
	raw := os.Getenv(guestcfg.EnvVar)
	os.Unsetenv(guestcfg.EnvVar)

	var (
		user     string
		keepHome bool
	)
	if raw != "" {
		cfg, err := guestcfg.Decode(raw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gvx: warning: container networks are not available: %v\n", err)
		} else {
			user, keepHome = cfg.User, cfg.KeepHome
			if cfg.HasLink() {
				if err := setupLink(cfg); err != nil {
					fmt.Fprintf(os.Stderr, "gvx: warning: container networks are not available: %v\n", err)
				}
			}
			if err := addHosts(cfg.Hosts); err != nil {
				fmt.Fprintf(os.Stderr, "gvx: warning: extra hosts are not in /etc/hosts: %v\n", err)
			}
			if cfg.Isolated {
				if err := isolate(isolatedKeep); err != nil {
					fmt.Fprintf(os.Stderr, "gvx: warning: the container is not cut off from the internet: %v\n", err)
				}
			}
		}
	}

	if user != "" {
		if err := dropTo(user, keepHome); err != nil {
			fmt.Fprintf(os.Stderr, "netinit: %v\n", err)
			os.Exit(126)
		}
	}

	argv := os.Args[1:]
	bin := argv[0]
	if !strings.Contains(bin, "/") {
		p, err := exec.LookPath(bin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "exec: %q: executable file not found in $PATH\n", bin)
			os.Exit(127)
		}
		bin = p
	}
	err := syscall.Exec(bin, argv, os.Environ())
	fmt.Fprintf(os.Stderr, "exec %s: %v\n", argv[0], err)
	if err == syscall.ENOENT {
		os.Exit(127)
	}
	os.Exit(126)
}

// isolatedKeep are the addresses an isolated container still reaches
// through the NAT: its host, where the hub listens, and its DNS relay.
var isolatedKeep = []netip.Addr{netip.AddrFrom4([4]byte{10, 0, 2, 2}), netip.AddrFrom4([4]byte{10, 0, 2, 3})}

// addHosts appends lines to /etc/hosts.
func addHosts(lines []string) error {
	if len(lines) == 0 {
		return nil
	}
	f, err := os.OpenFile("/etc/hosts", os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	_, werr := f.WriteString(b.String())
	return errors.Join(werr, f.Close())
}

func dropTo(spec string, keepHome bool) error {
	passwd, _ := os.ReadFile("/etc/passwd")
	group, _ := os.ReadFile("/etc/group")
	id, err := guestcfg.ResolveUser(spec, string(passwd), string(group))
	if err != nil {
		return err
	}
	groups := make([]int, 0, len(id.Groups))
	for _, g := range id.Groups {
		groups = append(groups, int(g))
	}
	if err := syscall.Setgroups(groups); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setgid(int(id.GID)); err != nil {
		return fmt.Errorf("setgid %d: %w", id.GID, err)
	}
	if err := syscall.Setuid(int(id.UID)); err != nil {
		return fmt.Errorf("setuid %d: %w", id.UID, err)
	}
	if !keepHome {
		os.Setenv("HOME", id.Home)
	}
	return nil
}
