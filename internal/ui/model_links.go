package ui

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/dockercli"
	"github.com/ironpark/gvx-desktop/internal/kubeapi"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// The contexts the app sets up: docker's for the Docker Engine, kubectl's
// for the k3s cluster. kubectl's is not "gvx" alone, so that it does not
// take the place of a context the user named so.
const (
	DockerContextName = "gvx"
	KubeContextName   = "gvx-k3s"
)

// CLIContext is what the app found of how a command-line tool on this
// machine reaches a recipe's engine: through a context of the tool's, named
// for gvx. The tool is asked in the background; see Model.CheckLink.
type CLIContext struct {
	Checked bool   // the tool was asked
	Busy    bool   // the app is changing the context
	CLI     string // the tool's path; empty when it was not found
	Err     string // the tool did not answer
	Pending bool   // there is nothing to point at yet: k3s has written no kubeconfig
	Exists  bool   // the tool has the gvx context
	Host    string // where the gvx context points
	Stale   bool   // the gvx context no longer matches the engine
	InUse   bool   // the gvx context is the tool's current one
	Current string // the tool's current context; empty when it has none
	// Contexts are the names of the tool's contexts.
	Contexts []string
	// Env names a variable of the app's environment that, in a terminal,
	// overrides the context the tool's configuration chooses.
	Env string
}

// Equal makes CLIContext comparable for signals.
func (c CLIContext) Equal(o CLIContext) bool {
	if !slices.Equal(c.Contexts, o.Contexts) {
		return false
	}
	c.Contexts, o.Contexts = nil, nil
	return reflect.DeepEqual(c, o)
}

// ContextStatus sums a CLIContext up: what the tool does now, and so which
// one action the app offers.
type ContextStatus int

const (
	ContextUnknown   ContextStatus = iota // not asked yet
	ContextNoCLI                          // the tool was not found
	ContextPending                        // the engine has not written what to point at
	ContextFailed                         // the tool did not answer
	ContextMissing                        // no gvx context: set it up and use it
	ContextStale                          // the gvx context points elsewhere: update it
	ContextElsewhere                      // the gvx context is right, another is in use: use it
	ContextInUse                          // the tool uses the gvx context: offer the way back
)

// Status sums c up. want is where the gvx context should point, when the
// tool's context names an address that can be compared; empty leaves it to
// Stale.
func (c CLIContext) Status(want string) ContextStatus {
	switch {
	case !c.Checked:
		return ContextUnknown
	case c.CLI == "":
		return ContextNoCLI
	case c.Pending:
		return ContextPending
	case c.Err != "":
		return ContextFailed
	case !c.Exists:
		return ContextMissing
	case c.Stale || want != "" && !sameEndpoint(c.Host, want):
		return ContextStale
	case !c.InUse:
		return ContextElsewhere
	}
	return ContextInUse
}

// sameEndpoint compares two addresses as docker does: a slash at the end
// changes nothing.
func sameEndpoint(a, b string) bool { return strings.TrimRight(a, "/") == strings.TrimRight(b, "/") }

// back is the context to switch back to from gvx's: the one in use when
// the app switched, while it is still there, and fallback otherwise.
func (c CLIContext) back(name, before, fallback string) string {
	if before != "" && before != name && slices.Contains(c.Contexts, before) {
		return before
	}
	return fallback
}

// LinkView is a CLIContext as the screens and menus show it.
type LinkView struct {
	C      CLIContext
	Want   string // where the gvx context should point, when that is an address
	Back   string // the context to switch back to; empty for none
	Status ContextStatus
}

// Equal makes LinkView comparable for signals.
func (v LinkView) Equal(o LinkView) bool {
	return v.C.Equal(o.C) && v.Want == o.Want && v.Back == o.Back && v.Status == o.Status
}

// A Link is a command-line tool on this machine and the context the app
// keeps in it for a recipe's engine: docker's gvx, kubectl's gvx-k3s.
type Link struct {
	Tool   string // the command, as a terminal names it
	Name   string // the context the app sets up in it
	Recipe string // the recipe whose engine it reaches
	State  *ggui.StateValue[CLIContext]
	View   *ggui.DerivedValue[LinkView]

	path      func(store.Settings) string // the tool the settings name; empty: find it
	read      func(ctx context.Context, cli string, run cliRunner) CLIContext
	write     func() linkWrite // sets the context up, worded on the UI thread
	use       func(name string) []string
	unset     []string // leaves the tool with no current context
	fallback  string   // the context to switch back to when the app knows none
	before    func(store.Settings) string
	setBefore func(*store.Settings, string)

	checking, again bool // a check runs; another is wanted after it
}

// linkWrite creates or updates a link's context in the background, running
// the tool with run.
type linkWrite func(ctx context.Context, run func(args ...string) ([]byte, error)) error

// cliRunner runs a command-line tool and returns what it wrote to its
// standard output; an error carries what it wrote to its standard error.
// Tests replace it.
type cliRunner func(ctx context.Context, path string, args ...string) ([]byte, error)

// overrides are the variables that make a tool in a terminal ignore the
// context its configuration chooses, or read another configuration.
var overrides = []string{"DOCKER_HOST", "DOCKER_CONTEXT", "KUBECONFIG"}

// runCLI runs a tool for the app, without the variables that override its
// configuration: the app reads and changes what a terminal without them
// uses, which is what docker context use and kubectl config use-context
// change.
func runCLI(ctx context.Context, path string, args ...string) ([]byte, error) {
	cmd := rungv.ToolCommand(ctx, path, args...)
	cmd.Env = withoutOverrides(os.Environ())
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return stdout.Bytes(), errors.New(msg)
		}
		return stdout.Bytes(), err
	}
	return stdout.Bytes(), nil
}

// withoutOverrides is env without the variables of overrides. Windows
// spells a variable's name in any case.
func withoutOverrides(env []string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.ContainsFunc(overrides, func(o string) bool { return strings.EqualFold(name, o) })
	})
}

// envOverride names the first of vars that lookup finds set.
func envOverride(lookup func(string) (string, bool), vars ...string) string {
	for _, v := range vars {
		if val, ok := lookup(v); ok && strings.TrimSpace(val) != "" {
			return v
		}
	}
	return ""
}

// newLinks makes the docker and kubectl links. Call it from NewModel.
func (m *Model) newLinks() {
	m.DockerLink = &Link{
		Tool: "docker", Name: DockerContextName, Recipe: recipes.Docker,
		State:    ggui.State(CLIContext{}),
		path:     func(s store.Settings) string { return s.DockerPath },
		read:     readDockerContext,
		write:    m.dockerWrite,
		use:      func(name string) []string { return []string{"context", "use", name} },
		fallback: "default",
		before:   func(s store.Settings) string { return s.DockerContextBefore },
		setBefore: func(s *store.Settings, v string) {
			s.DockerContextBefore = v
		},
	}
	m.KubeLink = &Link{
		Tool: "kubectl", Name: KubeContextName, Recipe: recipes.Kubernetes,
		State:     ggui.State(CLIContext{}),
		path:      func(store.Settings) string { return "" },
		read:      readKubeContext,
		write:     func() linkWrite { return kubeWrite(recipes.Kubeconfig()) },
		use:       func(name string) []string { return []string{"config", "use-context", name} },
		unset:     []string{"config", "unset", "current-context"},
		before:    func(s store.Settings) string { return s.KubeContextBefore },
		setBefore: func(s *store.Settings, v string) { s.KubeContextBefore = v },
	}
	m.DockerLink.View = ggui.Derived(func() LinkView {
		return m.DockerLink.view(m.DockerEndpoint.Get(), m.Settings.Get())
	}).WithEqual(LinkView.Equal)
	m.KubeLink.View = ggui.Derived(func() LinkView {
		return m.KubeLink.view("", m.Settings.Get())
	}).WithEqual(LinkView.Equal)
}

func (l *Link) view(want string, set store.Settings) LinkView {
	c := l.State.Get()
	return LinkView{C: c, Want: want, Back: c.back(l.Name, l.before(set), l.fallback), Status: c.Status(want)}
}

// Links are the app's links, docker's first.
func (m *Model) Links() []*Link { return []*Link{m.DockerLink, m.KubeLink} }

// CheckLink asks the link's tool, in the background, which contexts it has
// and which it uses. A check asked for while one runs runs after it.
func (m *Model) CheckLink(l *Link) {
	if m.Offline {
		return // the state shown is made up
	}
	if l.checking {
		l.again = true
		return
	}
	l.checking = true
	setting := l.path(ggui.Peek(m.Settings))
	run := m.runCLI
	var c CLIContext
	m.async(func(ctx context.Context) error {
		c = readLink(ctx, l, setting, run)
		return nil
	}, func(error) {
		l.checking = false
		c.Busy = ggui.Peek(l.State).Busy
		l.State.Set(c)
		if l == m.DockerLink {
			m.refreshBundledDockerState()
		}
		if l.again {
			l.again = false
			m.CheckLink(l)
		}
	})
}

// readLink finds the link's tool and asks it. It runs in the background.
func readLink(ctx context.Context, l *Link, setting string, run cliRunner) CLIContext {
	env := envOverride(os.LookupEnv, overridesOf(l.Tool)...)
	cli, ok := findCLI(l.Tool, setting)
	if !ok {
		return CLIContext{Checked: true, Env: env}
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c := l.read(ctx, cli, run)
	c.Checked, c.CLI, c.Env = true, cli, env
	return c
}

func overridesOf(tool string) []string {
	if tool == "kubectl" {
		return []string{"KUBECONFIG"}
	}
	return []string{"DOCKER_HOST", "DOCKER_CONTEXT"}
}

// findCLI finds a tool: the one the settings name, otherwise where a terminal
// would find it, and for docker falls back to the CLI bundled with GVX Desktop
// when no system docker is installed.
func findCLI(tool, setting string) (string, bool) {
	if set := strings.TrimSpace(setting); set != "" {
		p := rungv.ExpandHome(set)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, true
		}
		return "", false // a path that names nothing is not quietly replaced
	}
	// Not exec.LookPath alone: started from the Dock the app has a PATH
	// without Homebrew's directory, where brew install puts these.
	if p, err := rungv.FindTool(tool); err == nil {
		return p, true
	}
	if tool == "docker" {
		if b := dockercli.Bundled(); b.Available() {
			if managed, err := dockercli.ManagedDockerPath(); err == nil {
				if st, err := os.Stat(managed); err == nil && !st.IsDir() {
					return managed, true
				}
			}
			return b.Docker, true
		}
	}
	return "", false
}

// SetUpLink creates the link's context, or brings it up to date, and uses
// it.
func (m *Model) SetUpLink(l *Link) {
	write := l.write()
	m.changeLink(l, func(ctx context.Context, run func(...string) ([]byte, error)) error {
		if err := write(ctx, run); err != nil {
			return err
		}
		_, err := run(l.use(l.Name)...)
		return err
	}, true, tr("Could not set up the %s context", l.Tool), func(back string) {
		m.toastOK(tr("%s now uses %s", l.Tool, l.Name), m.backHint(l, back))
		if l == m.DockerLink && ggui.Peek(m.Settings).SandboxEngine {
			m.syncSandboxBuilder(true)
		}
	})
}

// UpdateLink points the link's context at the engine again, leaving which
// context is in use as it is.
func (m *Model) UpdateLink(l *Link) {
	m.changeLink(l, l.write(), false, tr("Could not update the %s context", l.Tool), func(string) {
		m.toastOK(tr("Updated the %s context", l.Name), "")
	})
}

// UseLink makes the link's context the one its tool uses.
func (m *Model) UseLink(l *Link) {
	m.changeLink(l, func(ctx context.Context, run func(...string) ([]byte, error)) error {
		_, err := run(l.use(l.Name)...)
		return err
	}, true, tr("Could not switch %s to %s", l.Tool, l.Name), func(back string) {
		m.toastOK(tr("%s now uses %s", l.Tool, l.Name), m.backHint(l, back))
		if l == m.DockerLink && ggui.Peek(m.Settings).SandboxEngine {
			m.syncSandboxBuilder(true)
		}
	})
}

// SwitchLinkBack makes the tool use the context it used before the app
// switched it, or none when there is none to go back to.
func (m *Model) SwitchLinkBack(l *Link) {
	v := ggui.Peek(l.View)
	args := l.unset
	if v.Back != "" {
		args = l.use(v.Back)
	}
	if args == nil {
		return
	}
	m.changeLink(l, func(ctx context.Context, run func(...string) ([]byte, error)) error {
		_, err := run(args...)
		return err
	}, false, tr("Could not switch %s back", l.Tool), func(string) {
		m.UpdateSettings(func(s *store.Settings) { l.setBefore(s, "") })
		if v.Back == "" {
			m.toastOK(tr("%s uses no context now", l.Tool), "")
			return
		}
		m.toastOK(tr("%s uses %s again", l.Tool, v.Back), "")
	})
}

// backHint tells how to switch back by hand.
func (m *Model) backHint(l *Link, back string) string {
	if back == "" {
		return ""
	}
	return tr("To switch back, run %s.", l.Tool+" "+strings.Join(l.use(back), " "))
}

// changeLink runs work with the link's tool in the background, then asks
// the tool again. switching says work makes the gvx context the current
// one: the context in use before it is remembered to switch back to.
func (m *Model) changeLink(l *Link, work linkWrite, switching bool, failed string, done func(back string)) {
	before := ggui.Peek(l.View)
	if before.C.Busy || m.Offline {
		return
	}
	cli := before.C.CLI
	if cli == "" {
		var ok bool
		if cli, ok = findCLI(l.Tool, l.path(ggui.Peek(m.Settings))); !ok {
			m.toastError(tr("%s was not found", l.Tool), errors.New(tr("install it, or name it in Settings, then try again")))
			return
		}
	}
	l.State.Update(func(c CLIContext) CLIContext { c.Busy = true; return c })
	run := m.runCLI
	m.async(func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return work(ctx, func(args ...string) ([]byte, error) { return run(ctx, cli, args...) })
	}, func(err error) {
		l.State.Update(func(c CLIContext) CLIContext { c.Busy = false; return c })
		if err != nil {
			m.toastError(failed, err)
		} else {
			back := before.Back
			if switching && !before.C.InUse {
				// The context in use until now is the one to go back to.
				back = before.C.Current
				if back != l.Name {
					m.UpdateSettings(func(s *store.Settings) { l.setBefore(s, back) })
				}
				if back == "" {
					back = l.fallback
				}
			}
			done(back)
		}
		m.CheckLink(l)
	})
}

// ---- docker -------------------------------------------------------------

// dockerContextLine is a line of docker context ls --format '{{json .}}',
// which every docker with contexts (19.03 on) writes.
type dockerContextLine struct {
	Name           string
	DockerEndpoint string
	Current        bool
	Error          string
}

// parseDockerContexts reads docker context ls --format '{{json .}}': one
// object a line. A JSON list, as some versions write for --format json,
// is read too, and lines that are not JSON, such as warnings, are passed.
func parseDockerContexts(out []byte, name string) (CLIContext, error) {
	var items []dockerContextLine
	for line := range bytes.SplitSeq(out, []byte("\n")) {
		line = bytes.TrimSpace(line)
		var err error
		switch {
		case bytes.HasPrefix(line, []byte("[")):
			var list []dockerContextLine
			err = json.Unmarshal(line, &list)
			items = append(items, list...)
		case bytes.HasPrefix(line, []byte("{")):
			var it dockerContextLine
			err = json.Unmarshal(line, &it)
			items = append(items, it)
		}
		if err != nil {
			return CLIContext{}, err
		}
	}
	if len(items) == 0 {
		return CLIContext{}, errors.New(tr("docker listed no contexts"))
	}
	var c CLIContext
	for _, it := range items {
		c.Contexts = append(c.Contexts, it.Name)
		if it.Current {
			c.Current = it.Name
		}
		if it.Name == name {
			c.Exists, c.Host = true, it.DockerEndpoint
		}
	}
	c.InUse = c.Exists && c.Current == name
	return c, nil
}

// readDockerContext asks docker for its contexts.
func readDockerContext(ctx context.Context, cli string, run cliRunner) CLIContext {
	out, err := run(ctx, cli, "context", "ls", "--format", "{{json .}}")
	if err != nil {
		return CLIContext{Err: firstLine(err.Error())}
	}
	c, err := parseDockerContexts(out, DockerContextName)
	if err != nil {
		return CLIContext{Err: firstLine(err.Error())}
	}
	if c.Current == "" {
		// A docker that marks no context current still names it.
		if out, err := run(ctx, cli, "context", "show"); err == nil {
			c.Current = lastLine(string(out))
			c.InUse = c.Exists && c.Current == DockerContextName
		}
	}
	return c
}

// dockerWrite creates the gvx context, or points it at the engine again.
func (m *Model) dockerWrite() linkWrite {
	host := m.dockerHost()
	return func(ctx context.Context, run func(...string) ([]byte, error)) error {
		_, err := run("context", "create", DockerContextName, "--docker", "host="+host)
		if err != nil && strings.Contains(err.Error(), "already exists") {
			_, err = run("context", "update", DockerContextName, "--docker", "host="+host)
		}
		return err
	}
}

// ---- kubectl ------------------------------------------------------------

// kubeconfig is what kubectl config view --raw -o json writes: the
// configuration in JSON, its certificates and keys in base64.
type kubeconfig = kubeapi.Config

func parseKubeconfig(out []byte) (kubeconfig, error) { return kubeapi.ParseConfig(out) }

// kubeLink compares the user's kubeconfig, mine, with the one the cluster
// wrote: whether mine has the gvx-k3s context, and whether that reaches
// this cluster with its credentials.
func kubeLink(mine, cluster kubeconfig) CLIContext {
	c := CLIContext{Current: mine.Current}
	for _, k := range mine.Contexts {
		c.Contexts = append(c.Contexts, k.Name)
	}
	have, ok := mine.Entry(KubeContextName)
	c.Exists, c.Host = ok, have.Server
	c.InUse = ok && mine.Current == KubeContextName
	if want, found := cluster.Entry(""); ok && found {
		c.Stale = have != want
	}
	return c
}

// readKubeContext asks kubectl for the user's kubeconfig and the
// cluster's, and compares them.
func readKubeContext(ctx context.Context, cli string, run cliRunner) CLIContext {
	file := recipes.Kubeconfig()
	if _, err := os.Stat(file); err != nil {
		return CLIContext{Pending: true}
	}
	view := func(args ...string) (kubeconfig, error) {
		out, err := run(ctx, cli, append(args, "config", "view", "--raw", "-o", "json")...)
		if err != nil {
			return kubeconfig{}, err
		}
		return parseKubeconfig(out)
	}
	mine, err := view()
	if err != nil {
		return CLIContext{Err: firstLine(err.Error())}
	}
	cluster, err := view("--kubeconfig", file)
	if err != nil {
		return CLIContext{Err: firstLine(err.Error())}
	}
	return kubeLink(mine, cluster)
}

// kubeWrite adds the cluster of the kubeconfig in file to the user's
// kubeconfig as the context gvx-k3s, with a cluster and a user of the same
// name, or brings them up to date. kubectl does the writing, so the other
// entries stay as they are; it writes the file anew, though, which drops
// its comments, so the file as it was before the first change is kept
// beside it. The certificates go to kubectl in files, which it embeds:
// never on a command line, which other users may read.
func kubeWrite(file string) linkWrite {
	return func(ctx context.Context, run func(...string) ([]byte, error)) error {
		out, err := run("--kubeconfig", file, "config", "view", "--raw", "-o", "json")
		if err != nil {
			return err
		}
		cfg, err := parseKubeconfig(out)
		if err != nil {
			return err
		}
		e, ok := cfg.Entry("")
		if !ok || e.Server == "" || e.Cert == "" || e.Key == "" {
			return errors.New(tr("the cluster's kubeconfig has no client certificate"))
		}
		if err := backUpKubeconfig(); err != nil {
			return err
		}
		dir, err := os.MkdirTemp("", "gvxd-kube")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		files := map[string]string{"ca.crt": e.CA, "client.crt": e.Cert, "client.key": e.Key}
		for name, data := range files {
			if data == "" {
				continue
			}
			b, err := base64.StdEncoding.DecodeString(data)
			if err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
				return err
			}
		}
		cluster := []string{"config", "set-cluster", KubeContextName, "--server=" + e.Server}
		if e.CA != "" {
			cluster = append(cluster, "--certificate-authority="+filepath.Join(dir, "ca.crt"), "--embed-certs=true")
		}
		for _, args := range [][]string{
			cluster,
			{"config", "set-credentials", KubeContextName, "--client-certificate=" + filepath.Join(dir, "client.crt"),
				"--client-key=" + filepath.Join(dir, "client.key"), "--embed-certs=true"},
			{"config", "set-context", KubeContextName, "--cluster=" + KubeContextName, "--user=" + KubeContextName},
		} {
			if _, err := run(args...); err != nil {
				return err
			}
		}
		return nil
	}
}

// UserKubeconfig is the kubeconfig kubectl reads when KUBECONFIG is not set.
func UserKubeconfig() string { return rungv.ExpandHome(filepath.Join("~", ".kube", "config")) }

// KubeconfigBackup is where the user's kubeconfig is kept as it was before
// the app first changed it.
func KubeconfigBackup() string { return UserKubeconfig() + ".before-gvx" }

// backUpKubeconfig copies the user's kubeconfig aside, once.
func backUpKubeconfig() error {
	if _, err := os.Stat(KubeconfigBackup()); err == nil {
		return nil
	}
	b, err := os.ReadFile(UserKubeconfig())
	if errors.Is(err, os.ErrNotExist) {
		return nil // kubectl makes one; there is nothing to keep
	}
	if err != nil {
		return err
	}
	return os.WriteFile(KubeconfigBackup(), b, 0o600)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
