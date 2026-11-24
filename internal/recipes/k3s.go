package recipes

import (
	"fmt"
	"slices"
	"strings"

	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// k3sAPIPort is where the k3s API server listens, in the sandbox and on
// the host.
const k3sAPIPort = 6443

// k3s's packaged load balancer, ServiceLB, gives a LoadBalancer service an
// address by taking the service's ports on every node, and the recipe's
// node is the sandbox: a port it takes reaches this computer only when the
// sandbox publishes it. With ServiceLB on, the recipe publishes the node's
// ports 80 and 443 on these.
const (
	ServiceLBHTTPPort  = 8080 // the host port of the node's port 80
	ServiceLBHTTPSPort = 8443 // the host port of the node's port 443
)

// serviceLBPorts are what the sandbox publishes for ServiceLB.
func serviceLBPorts() []string {
	return []string{
		fmt.Sprintf("127.0.0.1:%d:80", ServiceLBHTTPPort),
		fmt.Sprintf("127.0.0.1:%d:443", ServiceLBHTTPSPort),
	}
}

// k3sCommand is the server's command line. traefik is always off, and
// ServiceLB is unless serviceLB says otherwise. Its nodes pull the images
// of the local image registry as registries.yaml says.
func k3sCommand(serviceLB bool) string {
	disable := "traefik,servicelb"
	if serviceLB {
		disable = "traefik"
	}
	return "server --disable=" + disable + " --write-kubeconfig=/output/kubeconfig.yaml --write-kubeconfig-mode=644 " + privateRegistry()
}

// disabled reads the components a k3s command line turns off with
// --disable, which it may give more than once, as --disable=a,b or
// --disable a,b.
func disabled(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		v, ok := strings.CutPrefix(args[i], "--disable=")
		if !ok && args[i] == "--disable" && i+1 < len(args) {
			v, ok = args[i+1], true
			i++
		}
		if ok {
			for c := range strings.SplitSeq(v, ",") {
				if c = strings.TrimSpace(c); c != "" {
					out = append(out, c)
				}
			}
		}
	}
	return out
}

// ServiceLBOf reads from a cluster's sandbox whether ServiceLB runs in it:
// whether its command leaves it enabled.
func ServiceLBOf(spec sandbox.Spec) bool {
	args, err := rungv.SplitArgs(spec.Command)
	if err != nil {
		return false
	}
	return !slices.Contains(disabled(args), "servicelb")
}

// WithServiceLB returns the cluster's sandbox with ServiceLB on or off, its
// other flags and ports untouched: on, servicelb leaves --disable and the
// node's ports 80 and 443 are published, unless the sandbox publishes
// them already; off, the reverse. The change takes a restart.
func WithServiceLB(spec sandbox.Spec, on bool) sandbox.Spec {
	spec = spec.Clone()
	args, err := rungv.SplitArgs(spec.Command)
	if err != nil {
		return spec
	}
	var out []string
	added := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		v, isFlag := strings.CutPrefix(a, "--disable=")
		if !isFlag && a == "--disable" && i+1 < len(args) {
			v, isFlag = args[i+1], true
			i++
		}
		if !isFlag {
			out = append(out, a)
			continue
		}
		var keep []string
		for c := range strings.SplitSeq(v, ",") {
			if c = strings.TrimSpace(c); c != "" && c != "servicelb" {
				keep = append(keep, c)
			}
		}
		if !on && !added {
			keep = append(keep, "servicelb")
			added = true
		}
		if len(keep) > 0 {
			out = append(out, "--disable="+strings.Join(keep, ","))
		}
	}
	if !on && !added {
		// A command with no --disable: k3s runs its subcommand first.
		at := 0
		if len(out) > 0 && !strings.HasPrefix(out[0], "-") {
			at = 1
		}
		out = slices.Insert(out, at, "--disable=servicelb")
	}
	spec.Command = rungv.FormatArgs(out)

	defaults := serviceLBPorts()
	if on {
		for i, p := range defaults {
			guest := []int{80, 443}[i]
			if _, ok := HostAddress(spec, guest); !ok {
				spec.Ports = append(spec.Ports, p)
			}
		}
	} else {
		spec.Ports = slices.DeleteFunc(spec.Ports, func(p string) bool { return slices.Contains(defaults, strings.TrimSpace(p)) })
	}
	return spec
}

// LoadBalancerPorts are the ports of the node a LoadBalancer service can
// take and reach this computer through: none while ServiceLB is off, and
// with it on, those the sandbox publishes besides the API server's and the
// NodePorts.
func LoadBalancerPorts(spec sandbox.Spec) []int {
	if !ServiceLBOf(spec) {
		return nil
	}
	var out []int
	for _, r := range publishedTCP(spec) {
		for g := r.Guest; g < r.Guest+r.Len; g++ {
			if g != k3sAPIPort && (g < NodePortMin || g > NodePortMax) && !slices.Contains(out, g) {
				out = append(out, g)
			}
		}
	}
	return out
}
