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

// ImagesFull lists every cached image, dangling ones included, with full IDs
// and digests.
func (c *Client) ImagesFull(ctx context.Context) ([]Image, error) {
	out, err := c.output(ctx, "images", "-a", "--no-trunc", "--digests", "--format", "json")
	if err != nil {
		return nil, err
	}
	return decodeLines[Image](out)
}

// InspectImagesRaw returns the raw JSON array printed by rungv inspect for
// refs.
func (c *Client) InspectImagesRaw(ctx context.Context, refs ...string) ([]byte, error) {
	if len(refs) == 0 {
		return []byte("[]"), nil
	}
	for _, ref := range refs {
		if err := checkNoLeadingDash("image reference", ref); err != nil {
			return nil, err
		}
	}
	args := make([]string, 0, 2+len(refs))
	args = append(args, "inspect", "--")
	args = append(args, refs...)
	return c.output(ctx, args...)
}

// PullPlatform downloads ref for platform ("" for the host's default), handing
// each progress line to line.
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
	if platform != "" {
		args = append(args, "-platform", platform)
	}
	args = append(args, "--", ref)
	return c.Stream(ctx, line, args...)
}

// RemoveImageSafe removes a tag or image, terminating flags with "--" before ref.
func (c *Client) RemoveImageSafe(ctx context.Context, ref string, force bool) error {
	if err := checkNoLeadingDash("image reference", ref); err != nil {
		return err
	}
	args := []string{"rmi"}
	if force {
		args = append(args, "-f")
	}
	args = append(args, "--", ref)
	_, err := c.output(ctx, args...)
	return err
}

// TagImage gives source an additional reference target.
func (c *Client) TagImage(ctx context.Context, source, target string) error {
	if err := checkNoLeadingDash("source reference", source); err != nil {
		return err
	}
	if err := checkNoLeadingDash("target reference", target); err != nil {
		return err
	}
	_, err := c.output(ctx, "tag", "--", source, target)
	return err
}

// LoadImages reads a docker save or OCI archive from archive and hands each
// output line to line.
func (c *Client) LoadImages(ctx context.Context, archive io.Reader, line func(string)) error {
	if c.Path == "" {
		return ErrNotFound
	}
	if line == nil {
		line = func(string) {}
	}
	args := []string{"load"}
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
	if c.Path == "" {
		return ErrNotFound
	}
	if len(refs) == 0 {
		return errors.New("no images specified")
	}
	for _, ref := range refs {
		if err := checkNoLeadingDash("image reference", ref); err != nil {
			return err
		}
	}
	args := make([]string, 0, 2+len(refs))
	args = append(args, "save", "--")
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
