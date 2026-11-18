package ui

import (
	"context"
	"errors"
	"net"
	"slices"
	"strconv"
	"sync"
	"syscall"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/ui"

	"github.com/ironpark/gvx-desktop/internal/kubeapi"
	"github.com/ironpark/gvx-desktop/internal/recipes"
)

// Port forwards are ports of this computer the app listens on and carries
// to a port of a pod or a service of the cluster, as kubectl port-forward
// does, through the API server (kubeapi.Forward): no kubectl is needed.
// The app owns them. One runs until the user stops it, its pod or service
// goes, the cluster stops, or the app quits; none outlives the app, and
// none is started again with it.

// PortForward is a port forward that runs.
type PortForward struct {
	ID     int
	Target kubeapi.Target
	// UID is the pod's, for a pod: a pod made anew with the same name, as
	// a StatefulSet makes one, is another pod, and the forward ends.
	UID   string
	Local int // the port of 127.0.0.1 it listens on
	// Served counts the connections carried, and Err says why the last one
	// that could not be could not.
	Served int
	Err    string
}

// Address is where it listens.
func (f PortForward) Address() string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(f.Local)) }

// URL opens it in a browser.
func (f PortForward) URL() string { return "http://" + f.Address() }

// TargetText names what it reaches as kubectl does: pod/NAME:PORT or
// svc/NAME:PORT.
func (f PortForward) TargetText() string {
	if f.Target.Service != "" {
		return "svc/" + f.Target.Service + ":" + itoa(f.Target.Port)
	}
	return "pod/" + f.Target.Pod + ":" + itoa(f.Target.Port)
}

// kubectlForward is the kubectl command that forwards the same.
func kubectlForward(v LinkView, f PortForward) string {
	what := "pod/" + f.Target.Pod
	if f.Target.Service != "" {
		what = "svc/" + f.Target.Service
	}
	return kubectlCommand(v, "port-forward", "-n", f.Target.Namespace, what, itoa(f.Local)+":"+itoa(f.Target.Port))
}

// forwarder carries the connections ln accepts to t until ctx ends, and
// tells report of each. Tests replace it.
type forwarder func(ctx context.Context, t kubeapi.Target, ln net.Listener, report func(error)) error

// forwardThroughCluster forwards through the API server of the cluster the
// recipe's kubeconfig names.
func forwardThroughCluster(ctx context.Context, t kubeapi.Target, ln net.Listener, report func(error)) error {
	c, err := kubeapi.Open(recipes.Kubeconfig())
	if err != nil {
		ln.Close()
		return err
	}
	defer c.Close()
	return c.Forward(ctx, ln, t, report)
}

// liveForwards are the forwards that run, which the app stops as it quits.
// It is shared with the goroutines that run them.
type liveForwards struct {
	mu    sync.Mutex
	stops map[int]context.CancelFunc
	wg    sync.WaitGroup
	next  int
}

func (l *liveForwards) add(cancel context.CancelFunc) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stops == nil {
		l.stops = map[int]context.CancelFunc{}
	}
	l.next++
	l.stops[l.next] = cancel
	l.wg.Add(1)
	return l.next
}

// stop ends the forward id, if it runs.
func (l *liveForwards) stop(id int) {
	l.mu.Lock()
	cancel := l.stops[id]
	l.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// ended notes that the forward id's goroutine is done.
func (l *liveForwards) ended(id int) {
	l.mu.Lock()
	delete(l.stops, id)
	l.mu.Unlock()
	l.wg.Done()
}

// CloseForwards stops every port forward and waits for them to end: their
// ports are closed and the connections they carry cut. The app calls it as
// it quits.
func (m *Model) CloseForwards() {
	m.forwards.mu.Lock()
	for _, cancel := range m.forwards.stops {
		cancel()
	}
	m.forwards.mu.Unlock()
	m.forwards.wg.Wait()
}

// ForwardDraft is what the forward dialog forwards to: a pod or a service,
// with the ports it offers to choose from.
type ForwardDraft struct {
	Target kubeapi.Target // its Port is chosen in the dialog
	UID    string
	Ports  []ForwardPort
}

// ForwardPort is a port to offer: the ports a pod's containers declare, or
// a service's.
type ForwardPort struct {
	Port int
	Name string
}

func (d ForwardDraft) Equal(o ForwardDraft) bool {
	return d.Target == o.Target && d.UID == o.UID && slices.Equal(d.Ports, o.Ports)
}

// What names what the draft forwards to, as a heading says it.
func (d ForwardDraft) What() string {
	if d.Target.Service != "" {
		return "svc/" + d.Target.Service
	}
	return "pod/" + d.Target.Pod
}

// podForwardable reports whether a port of p can be forwarded to: it runs
// and is not being deleted. It need not be ready, as kubectl does not ask.
func podForwardable(p kubeapi.Pod) bool {
	return p.Status.Phase == "Running" && p.Metadata.Deleted == nil
}

// AskForwardPod opens the forward dialog on a pod.
func (m *Model) AskForwardPod(p kubeapi.Pod) {
	var ports []ForwardPort
	for _, cp := range p.Ports() {
		if (cp.Protocol == "" || cp.Protocol == "TCP") && !slices.ContainsFunc(ports, func(f ForwardPort) bool { return f.Port == cp.Port }) {
			ports = append(ports, ForwardPort{cp.Port, cp.Name})
		}
	}
	m.openForward(ForwardDraft{Target: kubeapi.Target{Namespace: p.Namespace(), Pod: p.Name()}, UID: p.Metadata.UID, Ports: ports})
}

// AskForwardService opens the forward dialog on a service.
func (m *Model) AskForwardService(s kubeapi.Service) {
	var ports []ForwardPort
	for _, sp := range s.Spec.Ports {
		if sp.Protocol == "" || sp.Protocol == "TCP" {
			ports = append(ports, ForwardPort{sp.Port, sp.Name})
		}
	}
	m.openForward(ForwardDraft{Target: kubeapi.Target{Namespace: s.Namespace(), Service: s.Name()}, Ports: ports})
}

func (m *Model) openForward(d ForwardDraft) {
	m.ForwardDraft.Set(d)
	m.ForwardErr.Set("")
	m.ForwardRemote.Set("")
	m.ForwardLocal.Set("")
	if len(d.Ports) > 0 {
		m.PickForwardPort(d.Ports[0].Port)
	}
	m.ForwardOpen.Set(true)
}

// PickForwardPort chooses the remote port of the dialog, and a local port
// for it.
func (m *Model) PickForwardPort(port int) {
	m.ForwardRemote.Set(itoa(port))
	m.ForwardLocal.Set(itoa(m.suggestLocal(port)))
	m.ForwardErr.Set("")
}

// suggestLocal is the local port offered for a remote one: the same
// number, or past 8000 for one below 1024, which only an administrator
// may take on some systems; or the next after it that is free.
func (m *Model) suggestLocal(remote int) int {
	want := defaultLocal(remote)
	for p := want; p < want+100 && p <= 65535; p++ {
		if !m.localTaken(p) {
			return p
		}
	}
	return want
}

// defaultLocal is the local port first offered for a remote one.
func defaultLocal(remote int) int {
	if remote < 1024 {
		return remote + 8000
	}
	return remote
}

// localTaken reports whether a port of 127.0.0.1 is taken: by a forward
// of the app's, or by any program, which trying it finds out.
func (m *Model) localTaken(port int) bool {
	if slices.ContainsFunc(ggui.Peek(m.Forwards), func(f PortForward) bool { return f.Local == port }) {
		return true
	}
	if m.Offline {
		return false // nothing is tried
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return true
	}
	ln.Close()
	return false
}

// parsePort reads a port typed in a field.
func parsePort(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	return n, err == nil && n >= 1 && n <= 65535
}

// SubmitForward starts the forward the dialog describes, or says in it
// why it cannot.
func (m *Model) SubmitForward() {
	d := ggui.Peek(m.ForwardDraft)
	remote, ok := parsePort(ggui.Peek(m.ForwardRemote))
	if !ok {
		m.ForwardErr.Set(tr("The port to forward to is a number from 1 to 65535."))
		return
	}
	local, ok := parsePort(ggui.Peek(m.ForwardLocal))
	if !ok {
		m.ForwardErr.Set(tr("The local port is a number from 1 to 65535."))
		return
	}
	t := d.Target
	t.Port = remote
	f, err := m.StartForward(t, d.UID, local)
	if err != nil {
		m.ForwardErr.Set(err.Error())
		return
	}
	m.ForwardOpen.Set(false)
	if f.ID != 0 {
		m.toastOK(tr("Forwarding %s", f.Address()), tr("Connections to it reach %s.", f.TargetText()))
	}
}

// StartForward listens on local and forwards it to t. Made up, as the
// screens are, it starts nothing and answers a forward with no ID.
func (m *Model) StartForward(t kubeapi.Target, uid string, local int) (PortForward, error) {
	if m.Offline {
		return PortForward{}, nil
	}
	for _, f := range ggui.Peek(m.Forwards) {
		if f.Local == local {
			return PortForward{}, errors.New(tr("Port %d is forwarded already, to %s.", local, f.TargetText()))
		}
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(local)))
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return PortForward{}, errors.New(tr("Port %d of this computer is in use by another program. Choose another.", local))
		}
		return PortForward{}, errors.New(tr("Could not listen on port %d: %s", local, err.Error()))
	}
	ctx, cancel := context.WithCancel(context.Background())
	id := m.forwards.add(cancel)
	f := PortForward{ID: id, Target: t, UID: uid, Local: ln.Addr().(*net.TCPAddr).Port}
	m.Forwards.Set(append(slices.Clone(ggui.Peek(m.Forwards)), f))
	run := m.forwarder
	go func() {
		defer m.forwards.ended(id)
		err := run(ctx, t, ln, func(err error) { m.post(func() { m.forwardReported(id, err) }) })
		m.post(func() { m.forwardEnded(id, err) })
	}()
	return f, nil
}

// forwardReported notes how a connection the forward id carried went.
func (m *Model) forwardReported(id int, err error) {
	m.Forwards.Update(func(list []PortForward) []PortForward {
		list = slices.Clone(list)
		for i := range list {
			if list[i].ID != id {
				continue
			}
			if err != nil {
				list[i].Err = err.Error()
			} else {
				list[i].Served++
				list[i].Err = ""
			}
		}
		return list
	})
}

// forwardEnded drops a forward that ended on its own, as when its port
// stopped taking connections, and says so.
func (m *Model) forwardEnded(id int, err error) {
	f, ok := m.dropForward(id)
	if !ok {
		return // stopped
	}
	if err == nil {
		err = errors.New(tr("It stopped taking connections."))
	}
	m.toastError(tr("Port forward %s stopped", f.Address()), err)
}

func (m *Model) dropForward(id int) (PortForward, bool) {
	list := ggui.Peek(m.Forwards)
	i := slices.IndexFunc(list, func(f PortForward) bool { return f.ID == id })
	if i < 0 {
		return PortForward{}, false
	}
	f := list[i]
	m.Forwards.Set(slices.Delete(slices.Clone(list), i, i+1))
	return f, true
}

// StopForward stops a port forward; why, when given, is told in a toast.
func (m *Model) StopForward(id int, why string) {
	f, ok := m.dropForward(id)
	m.forwards.stop(id)
	if ok && why != "" {
		m.Toasts.Push(ui.Toast(tr("Port forward %s stopped", f.Address()), why).Info())
	}
}

// StopForwards stops them all, saying why.
func (m *Model) StopForwards(why string) {
	for _, f := range ggui.Peek(m.Forwards) {
		m.StopForward(f.ID, why)
	}
}

// checkForwards stops the forwards whose pod or service is gone, by what
// the cluster just listed: its pods, and its services when fresh says they
// were listed too.
func (m *Model) checkForwards(pods []kubeapi.Pod, services []kubeapi.Service, fresh bool) {
	for _, f := range ggui.Peek(m.Forwards) {
		t := f.Target
		switch {
		case t.Pod != "":
			i := slices.IndexFunc(pods, func(p kubeapi.Pod) bool { return p.Namespace() == t.Namespace && p.Name() == t.Pod })
			if i < 0 || f.UID != "" && pods[i].Metadata.UID != f.UID {
				m.StopForward(f.ID, tr("Pod %s is gone.", t.Pod))
			}
		case fresh:
			if !slices.ContainsFunc(services, func(s kubeapi.Service) bool { return s.Namespace() == t.Namespace && s.Name() == t.Service }) {
				m.StopForward(f.ID, tr("Service %s is gone.", t.Service))
			}
		}
	}
}

// followForwards stops the forwards as the cluster stops. Call it from
// Start.
func (m *Model) followForwards() {
	ggui.Watch(m.PodsOpen, func(open bool) {
		if !open && len(ggui.Peek(m.Forwards)) > 0 {
			m.StopForwards(tr("The cluster stopped."))
		}
	})
}
