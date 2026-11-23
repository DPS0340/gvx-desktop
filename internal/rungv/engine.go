package rungv

// Owner: workstream C (images, volumes, networks, system).
// Helpers the experimental sandbox engine uses on top of Client.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
)

func checkNoLeadingDash(label, val string) error {
	if strings.HasPrefix(val, "-") {
		return fmt.Errorf("invalid %s %q: must not start with '-'", label, val)
	}
	return nil
}

func checkNoControlChars(label, val string) error {
	for i := range len(val) {
		if val[i] < 0x20 || val[i] == 0x7f {
			return fmt.Errorf("invalid %s %q: contains control character", label, val)
		}
	}
	return nil
}

func validateFlagValue(label, val string) error {
	if err := checkNoLeadingDash(label, val); err != nil {
		return err
	}
	return checkNoControlChars(label, val)
}

func validateArchFlag(arch string) error {
	if arch == "" {
		return nil
	}
	if err := checkNoLeadingDash("arch", arch); err != nil {
		return err
	}
	for i := range len(arch) {
		if arch[i] <= 0x20 || arch[i] == 0x7f {
			return fmt.Errorf("invalid arch %q", arch)
		}
	}
	return nil
}

// archFromPlatform extracts the Linux architecture ("amd64", "arm64", ...)
// from a Docker platform string such as "linux/amd64" or "linux/arm64/v8".
func archFromPlatform(platform string) string {
	p := strings.TrimSpace(strings.ToLower(platform))
	if p == "" {
		return ""
	}
	parts := strings.Split(p, "/")
	if len(parts) >= 2 && parts[0] == "linux" {
		switch parts[1] {
		case "x86_64":
			return "amd64"
		case "aarch64":
			return "arm64"
		default:
			return parts[1]
		}
	}
	if len(parts) == 1 {
		switch parts[0] {
		case "amd64", "x86_64":
			return "amd64"
		case "arm64", "aarch64":
			return "arm64"
		}
	}
	return ""
}

// ImagesFull lists every cached image, dangling ones included, with full IDs
// and digests.
func (c *Client) ImagesFull(ctx context.Context) ([]Image, error) {
	return c.ImagesFullArch(ctx, "")
}

// ImagesFullArch lists every cached image in arch's namespace ("" for host default).
func (c *Client) ImagesFullArch(ctx context.Context, arch string) ([]Image, error) {
	if err := validateArchFlag(arch); err != nil {
		return nil, err
	}
	args := []string{"images"}
	if arch != "" {
		args = append(args, "-arch", arch)
	}
	args = append(args, "-a", "--no-trunc", "--digests", "--format", "json")
	out, err := c.output(ctx, args...)
	if err != nil {
		return nil, err
	}
	return decodeLines[Image](out)
}

// InspectImagesRaw returns the raw JSON array printed by rungv inspect for
// refs.
func (c *Client) InspectImagesRaw(ctx context.Context, refs ...string) ([]byte, error) {
	return c.InspectImagesRawArch(ctx, "", refs...)
}

// InspectImagesRawArch returns the raw JSON array printed by rungv inspect in
// arch's namespace ("" for host default).
func (c *Client) InspectImagesRawArch(ctx context.Context, arch string, refs ...string) ([]byte, error) {
	if len(refs) == 0 {
		return []byte("[]"), nil
	}
	if err := validateArchFlag(arch); err != nil {
		return nil, err
	}
	for _, ref := range refs {
		if err := checkNoLeadingDash("image reference", ref); err != nil {
			return nil, err
		}
	}
	args := make([]string, 0, 4+len(refs))
	args = append(args, "inspect")
	if arch != "" {
		args = append(args, "-arch", arch)
	}
	args = append(args, "--")
	args = append(args, refs...)
	return c.output(ctx, args...)
}

// PullPlatform downloads ref for platform ("" for the host's default), handing
// each progress line to line. When platform names an architecture, -arch is
// also passed so rungv stores the image in that architecture's namespace.
// When rungv supports -keep-blobs (v0.21+), it is passed automatically so
// cached images can be pushed and saved offline.
func (c *Client) PullPlatform(ctx context.Context, ref, platform string, line func(string)) error {
	if err := checkNoLeadingDash("image reference", ref); err != nil {
		return err
	}
	if platform != "" {
		if err := checkNoLeadingDash("platform", platform); err != nil {
			return err
		}
	}
	if line == nil {
		line = func(string) {}
	}
	args := []string{"pull"}
	if arch := archFromPlatform(platform); arch != "" {
		args = append(args, "-arch", arch)
	}
	if platform != "" {
		args = append(args, "-platform", platform)
	}
	if c.Features(ctx).KeepBlobs {
		args = append(args, "-keep-blobs")
	}
	args = append(args, "--", ref)
	return c.Stream(ctx, line, args...)
}

// PushOptions are the options for rungv push.
type PushOptions struct {
	Ref         string // cached image reference or ID (required)
	Target      string // optional destination reference
	Arch        string // optional architecture namespace (derived from Platform when empty)
	Platform    string // optional image platform (e.g. linux/amd64)
	RegistryURL string // optional OCI registry base URL override
	// AuthPayload, when non-empty, is passed over stdin with --auth-stdin
	// (Docker X-Registry-Auth JSON or base64 JSON). Leave empty to use the
	// Docker config store.
	AuthPayload string
	Quiet       bool
}

// PushImage uploads a cached OCI image to a registry via rungv push, streaming
// stdout and stderr lines to line.
func (c *Client) PushImage(ctx context.Context, opts PushOptions, line func(string)) error {
	if c == nil || c.Path == "" {
		return ErrNotFound
	}
	if strings.TrimSpace(opts.Ref) == "" {
		return errors.New("invalid image reference: must not be empty")
	}
	if err := validateFlagValue("image reference", opts.Ref); err != nil {
		return err
	}
	if opts.Target != "" {
		if err := validateFlagValue("target reference", opts.Target); err != nil {
			return err
		}
	}
	if err := validateArchFlag(opts.Arch); err != nil {
		return err
	}
	if opts.Platform != "" {
		if err := validateFlagValue("platform", opts.Platform); err != nil {
			return err
		}
	}
	if opts.RegistryURL != "" {
		if err := validateFlagValue("registry URL", opts.RegistryURL); err != nil {
			return err
		}
	}
	if line == nil {
		line = func(string) {}
	}
	arch := opts.Arch
	if arch == "" && opts.Platform != "" {
		arch = archFromPlatform(opts.Platform)
	}
	args := []string{"push"}
	if arch != "" {
		args = append(args, "-arch", arch)
	}
	if opts.Platform != "" {
		args = append(args, "-platform", opts.Platform)
	}
	if opts.RegistryURL != "" {
		args = append(args, "-registry-url", opts.RegistryURL)
	}
	if opts.Quiet {
		args = append(args, "-q")
	}
	if opts.AuthPayload != "" {
		args = append(args, "--auth-stdin")
	}
	args = append(args, "--", opts.Ref)
	if opts.Target != "" {
		args = append(args, opts.Target)
	}

	cmd := c.Command(ctx, args...)
	if opts.AuthPayload != "" {
		cmd.Stdin = strings.NewReader(opts.AuthPayload)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	var tail tailBuffer
	if err := cmd.Start(); err != nil {
		return err
	}
	var wg sync.WaitGroup
	read := func(r io.Reader, keep bool) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			text := sc.Text()
			if keep {
				tail.add(text)
			}
			line(text)
		}
	}
	wg.Add(2)
	go read(stdout, false)
	go read(stderr, true)
	wg.Wait()
	if err := cmd.Wait(); err != nil {
		e := &Error{Args: args, Stderr: tail.String(), Err: err}
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			e.Code = exit.ExitCode()
		}
		return e
	}
	return nil
}

// RemoveImageSafe removes a tag or image, terminating flags with "--" before ref.
func (c *Client) RemoveImageSafe(ctx context.Context, ref string, force bool) error {
	return c.RemoveImageSafeArch(ctx, "", ref, force)
}

// RemoveImageSafeArch removes a tag or image in arch's namespace.
func (c *Client) RemoveImageSafeArch(ctx context.Context, arch, ref string, force bool) error {
	if err := validateArchFlag(arch); err != nil {
		return err
	}
	if err := checkNoLeadingDash("image reference", ref); err != nil {
		return err
	}
	args := []string{"rmi"}
	if arch != "" {
		args = append(args, "-arch", arch)
	}
	if force {
		args = append(args, "-f")
	}
	args = append(args, "--", ref)
	_, err := c.output(ctx, args...)
	return err
}

// TagImage gives source an additional reference target.
func (c *Client) TagImage(ctx context.Context, source, target string) error {
	return c.TagImageArch(ctx, "", source, target)
}

// TagImageArch gives source an additional reference target in arch's namespace.
func (c *Client) TagImageArch(ctx context.Context, arch, source, target string) error {
	if err := validateArchFlag(arch); err != nil {
		return err
	}
	if err := checkNoLeadingDash("source reference", source); err != nil {
		return err
	}
	if err := checkNoLeadingDash("target reference", target); err != nil {
		return err
	}
	args := []string{"tag"}
	if arch != "" {
		args = append(args, "-arch", arch)
	}
	args = append(args, "--", source, target)
	_, err := c.output(ctx, args...)
	return err
}

// LoadImages reads a docker save or OCI archive from archive and hands each
// output line to line.
func (c *Client) LoadImages(ctx context.Context, archive io.Reader, line func(string)) error {
	return c.LoadImagesArch(ctx, "", archive, line)
}

// LoadImagesArch reads a docker save or OCI archive into arch's namespace.
func (c *Client) LoadImagesArch(ctx context.Context, arch string, archive io.Reader, line func(string)) error {
	if c.Path == "" {
		return ErrNotFound
	}
	if err := validateArchFlag(arch); err != nil {
		return err
	}
	if line == nil {
		line = func(string) {}
	}
	args := []string{"load"}
	if arch != "" {
		args = append(args, "-arch", arch)
	}
	if c.Features(ctx).KeepBlobs {
		args = append(args, "-keep-blobs")
	}
	cmd := c.Command(ctx, args...)
	cmd.Stdin = archive
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	var tail tailBuffer
	if err := cmd.Start(); err != nil {
		return err
	}
	var wg sync.WaitGroup
	read := func(r io.Reader, keep bool) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			text := sc.Text()
			if keep {
				tail.add(text)
			}
			line(text)
		}
	}
	wg.Add(2)
	go read(stdout, false)
	go read(stderr, true)
	wg.Wait()
	if err := cmd.Wait(); err != nil {
		e := &Error{Args: args, Stderr: tail.String(), Err: err}
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			e.Code = exit.ExitCode()
		}
		return e
	}
	return nil
}

// SaveImages writes refs as a single tar archive to w.
func (c *Client) SaveImages(ctx context.Context, w io.Writer, refs ...string) error {
	return c.SaveImagesArch(ctx, "", w, refs...)
}

// SaveImagesArch writes refs from arch's namespace as a single tar archive to w.
func (c *Client) SaveImagesArch(ctx context.Context, arch string, w io.Writer, refs ...string) error {
	if c.Path == "" {
		return ErrNotFound
	}
	if err := validateArchFlag(arch); err != nil {
		return err
	}
	if len(refs) == 0 {
		return errors.New("no images specified")
	}
	for _, ref := range refs {
		if err := checkNoLeadingDash("image reference", ref); err != nil {
			return err
		}
	}
	args := make([]string, 0, 4+len(refs))
	args = append(args, "save")
	if arch != "" {
		args = append(args, "-arch", arch)
	}
	args = append(args, "--")
	args = append(args, refs...)
	cmd := c.Command(ctx, args...)
	cmd.Stdout = w
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		e := &Error{Args: args, Stderr: stderr.String(), Err: err}
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			e.Code = exit.ExitCode()
		}
		return e
	}
	return nil
}

// PruneImagesArch deletes dangling images (or all with all=true) in arch's
// namespace.
func (c *Client) PruneImagesArch(ctx context.Context, arch string, all bool) (string, error) {
	if err := validateArchFlag(arch); err != nil {
		return "", err
	}
	args := []string{"image", "prune", "-f"}
	if arch != "" {
		args = append(args, "-arch", arch)
	}
	if all {
		args = append(args, "-a")
	}
	out, err := c.output(ctx, args...)
	return strings.TrimSpace(string(out)), err
}

// CreateVolumeSafe formats a named volume, terminating flags with "--" before name.
func (c *Client) CreateVolumeSafe(ctx context.Context, name, driver string) error {
	if err := checkNoLeadingDash("volume name", name); err != nil {
		return err
	}
	if driver != "" {
		if err := checkNoLeadingDash("volume driver", driver); err != nil {
			return err
		}
	}
	args := []string{"volume", "create"}
	if driver != "" {
		args = append(args, "-d", driver)
	}
	if name != "" {
		args = append(args, "--", name)
	}
	_, err := c.output(ctx, args...)
	return err
}

// RemoveVolumeSafe deletes an unlocked volume, terminating flags with "--" before name.
func (c *Client) RemoveVolumeSafe(ctx context.Context, name string) error {
	if err := checkNoLeadingDash("volume name", name); err != nil {
		return err
	}
	_, err := c.output(ctx, "volume", "rm", "--", name)
	return err
}
