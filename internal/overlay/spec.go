package overlay

import (
	"errors"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/ironpark/gvx-desktop/internal/overlay/guestcfg"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// NetinitUser is the user that the sandbox of a container started with
// netinit runs as: root, named outright. rungv runs a sandbox given no user
// as its image's USER, and netinit, run as a user such as that, can set up
// no network and switch to no user (setgroups: operation not permitted).
const NetinitUser = "0:0"

// WithNetinit returns spec running netinit (mounted read-only from binDir at
// guestcfg.MountDir) with cfg in front of its command, as root, and with the
// DNS server dns when it is valid. netinit drops to the spec's user (the
// container's, or else its image's) once the network is up.
func WithNetinit(spec sandbox.Spec, cfg guestcfg.Config, binDir string, dns netip.AddrPort) (sandbox.Spec, error) {
	if !spec.SetEntrypoint || spec.Entrypoint == "" {
		return spec, errors.New("the container has no command")
	}
	args, err := rungv.SplitArgs(spec.Command)
	if err != nil {
		return spec, err
	}
	cfg.User = spec.User
	cfg.KeepHome = slices.ContainsFunc(spec.Env, func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return k == "HOME"
	})
	enc, err := cfg.Encode()
	if err != nil {
		return spec, err
	}
	out := spec.Clone()
	out.Entrypoint = guestcfg.Path
	out.Command = rungv.FormatArgs(append([]string{spec.Entrypoint}, args...))
	out.User = NetinitUser
	out.Env = append(slices.DeleteFunc(slices.Clone(spec.Env), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return k == guestcfg.EnvVar
	}), guestcfg.EnvVar+"="+enc)
	out.Mounts = append(slices.Clone(spec.Mounts), sandbox.Mount{
		Type:     sandbox.MountBind,
		Source:   binDir,
		Target:   guestcfg.MountDir,
		ReadOnly: true,
	})
	if dns.IsValid() || cfg.HasLink() {
		extra, err := rungv.SplitArgs(spec.ExtraArgs)
		if err != nil {
			return spec, err
		}
		if dns.IsValid() {
			extra = append(extra, "-host-dns", dns.String())
		}
		if cfg.HasLink() && cfg.MTU > 0 {
			if nat := cfg.MTU + NATOverhead; nat > 1500 {
				extra = append(extra, "-tune", "net.nat.mtu="+strconv.Itoa(nat))
			}
		}
		out.ExtraArgs = rungv.FormatArgs(extra)
	}
	return out, nil
}
