//go:build !windows

package engine

// Real docker CLI verification of diff, export and commit, and of a stopped
// container keeping its file changes, against startHarness
// (GVX_ENGINE_HARNESS=1). It needs rungv v0.20.

import (
	"archive/tar"
	"bytes"
	"io"
	"slices"
	"strings"
	"testing"
)

func TestDiffExportCommitHarness(t *testing.T) {
	h := startHarness(t)
	if !h.e.runner.Features().Control {
		t.Skip("diff, export and commit need rungv v0.20")
	}
	t.Cleanup(func() {
		h.docker(t, "", "rm", "-f", "gvxd-fs", "gvxd-fs-run", "gvxd-fs-new")
		h.docker(t, "", "rmi", "gvxd-fs-img:1", "gvxd-fs-img:2")
	})
	mustDocker := func(args ...string) string {
		t.Helper()
		out, errOut, code := h.docker(t, "", args...)
		if code != 0 {
			t.Fatalf("docker %s: code %d\n%s%s", strings.Join(args, " "), code, out, errOut)
		}
		return out
	}
	diffOf := func(name string) map[string]string {
		t.Helper()
		m := map[string]string{}
		for line := range strings.SplitSeq(strings.TrimSpace(mustDocker("diff", name)), "\n") {
			if kind, p, ok := strings.Cut(line, " "); ok {
				m[p] = kind
			}
		}
		return m
	}
	wantDiff := func(what string, m map[string]string, want map[string]string, absent ...string) {
		t.Helper()
		for p, k := range want {
			if m[p] != k {
				t.Errorf("%s: %s is %q, want %q (diff: %v)", what, p, m[p], k, m)
			}
		}
		for _, p := range absent {
			if _, ok := m[p]; ok {
				t.Errorf("%s: %s is listed (diff: %v)", what, p, m)
			}
		}
	}

	// A container that ran and stopped: its changes are kept in its volume.
	mustDocker("run", "--name", "gvxd-fs", "alpine:3.20", "sh", "-c",
		"echo hi > /root/new.txt; echo x >> /etc/motd; rm /etc/issue; mkdir -p /opt/app; echo app > /opt/app/conf")
	wantDiff("stopped", diffOf("gvxd-fs"), map[string]string{
		"/etc": "C", "/etc/motd": "C", "/etc/issue": "D", "/root": "C", "/root/new.txt": "A",
		"/opt": "C", "/opt/app": "A", "/opt/app/conf": "A",
	}, "/etc/hosts", "/etc/hostname", "/etc/resolv.conf", "/workspace")

	// The changes outlive a stop: the second run appends to the motd the
	// first one changed.
	h.docker(t, "", "start", "-a", "gvxd-fs") // rm /etc/issue fails now; the rest runs

	// export of the stopped container: the image's files and the changes.
	exported, errOut, code := h.docker(t, "", "export", "gvxd-fs")
	if code != 0 {
		t.Fatalf("docker export: %d %s", code, errOut)
	}
	names := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader([]byte(exported)))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("the export is not a tar: %v", err)
		}
		b, _ := io.ReadAll(tr)
		names[strings.TrimSuffix(hdr.Name, "/")] = b
	}
	for _, n := range []string{"bin/busybox", "etc/os-release", "root/new.txt", "opt/app/conf"} {
		if _, ok := names[n]; !ok {
			t.Errorf("export lacks %s", n)
		}
	}
	if _, ok := names["etc/issue"]; ok {
		t.Error("export holds the deleted etc/issue")
	}
	if !strings.HasSuffix(string(names["etc/motd"]), "x\nx\n") {
		t.Errorf("export's etc/motd does not hold both runs' lines: %q", names["etc/motd"])
	}

	// commit of the stopped container, with a change, and a run of it.
	mustDocker("commit", "--change", `CMD ["cat", "/root/new.txt"]`, "gvxd-fs", "gvxd-fs-img:1")
	if got := strings.TrimSpace(mustDocker("run", "--rm", "gvxd-fs-img:1")); got != "hi" {
		t.Errorf("the committed image runs %q, want hi", got)
	}
	if got := strings.TrimSpace(mustDocker("run", "--rm", "gvxd-fs-img:1", "sh", "-c", "ls /etc/issue 2>&1; cat /opt/app/conf")); !strings.Contains(got, "No such file") || !strings.HasSuffix(got, "app") {
		t.Errorf("the committed image's files: %q", got)
	}
	if got := mustDocker("image", "inspect", "--format", "{{len .RootFS.Layers}}", "gvxd-fs-img:1"); strings.TrimSpace(got) != "2" {
		t.Errorf("the committed image has %s layers, want alpine's and one", got)
	}

	// A running container: diff, export and commit read it in the guest,
	// and a commit on top of a committed image (which rungv save cannot give).
	mustDocker("run", "-d", "--name", "gvxd-fs-run", "gvxd-fs-img:1", "sh", "-c", "echo more > /root/more; rm /root/new.txt; sleep 300")
	var running map[string]string
	for range 50 {
		if running = diffOf("gvxd-fs-run"); running["/root/more"] == "A" {
			break
		}
	}
	wantDiff("running", running, map[string]string{"/root": "C", "/root/more": "A", "/root/new.txt": "D"})
	exported, _, code = h.docker(t, "", "export", "gvxd-fs-run")
	if code != 0 || !strings.Contains(exported, "root/more") {
		t.Errorf("export of a running container: code %d, has root/more: %v", code, strings.Contains(exported, "root/more"))
	}
	mustDocker("commit", "gvxd-fs-run", "gvxd-fs-img:2")
	if got := mustDocker("inspect", "--format", "{{.State.Status}}", "gvxd-fs-run"); strings.TrimSpace(got) != "running" {
		t.Errorf("after commit the container is %s", got)
	}
	got := strings.TrimSpace(mustDocker("run", "--rm", "--name", "gvxd-fs-new", "gvxd-fs-img:2", "sh", "-c", "cat /root/more; ls /root"))
	if lines := strings.Split(got, "\n"); lines[0] != "more" || slices.Contains(lines, "new.txt") {
		t.Errorf("the second commit's files: %q", got)
	}
}
