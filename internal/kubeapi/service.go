package kubeapi

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// Service is the part of a v1 Service the app shows.
type Service struct {
	Metadata Meta          `json:"metadata"`
	Spec     ServiceSpec   `json:"spec"`
	Status   ServiceStatus `json:"status"`
}

// ServiceSpec is the part of a service's spec the app shows.
type ServiceSpec struct {
	Type         string            `json:"type"`
	ClusterIP    string            `json:"clusterIP,omitempty"`
	ExternalName string            `json:"externalName,omitempty"`
	Ports        []ServicePort     `json:"ports,omitempty"`
	Selector     map[string]string `json:"selector,omitempty"`
}

// ServicePort is a port of a service: the port it takes on its cluster IP,
// the port of its pods it leads to, and for a NodePort or LoadBalancer
// service the port it takes on each node.
type ServicePort struct {
	Name       string      `json:"name,omitempty"`
	Protocol   string      `json:"protocol,omitempty"`
	Port       int         `json:"port"`
	TargetPort IntOrString `json:"targetPort"`
	NodePort   int         `json:"nodePort,omitempty"`
}

// ServiceStatus is a load balancer's addresses, which a LoadBalancer
// service has once something gives it one.
type ServiceStatus struct {
	LoadBalancer struct {
		Ingress []struct {
			IP       string `json:"ip,omitempty"`
			Hostname string `json:"hostname,omitempty"`
		} `json:"ingress,omitempty"`
	} `json:"loadBalancer"`
}

// IntOrString is a port given by number or by the name of a container's
// port, as a service's targetPort is.
type IntOrString struct {
	Int int
	Str string
}

// UnmarshalJSON reads a number or a string.
func (v *IntOrString) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		v.Int = 0
		return json.Unmarshal(b, &v.Str)
	}
	v.Str = ""
	return json.Unmarshal(b, &v.Int)
}

// MarshalJSON writes it back as it was given.
func (v IntOrString) MarshalJSON() ([]byte, error) {
	if v.Str != "" {
		return json.Marshal(v.Str)
	}
	return json.Marshal(v.Int)
}

// String is the number or the name.
func (v IntOrString) String() string {
	if v.Str != "" {
		return v.Str
	}
	return strconv.Itoa(v.Int)
}

// Equal makes Service comparable for signals.
func (s Service) Equal(o Service) bool { return reflect.DeepEqual(s, o) }

// Name is the service's name.
func (s Service) Name() string { return s.Metadata.Name }

// Namespace is the service's namespace.
func (s Service) Namespace() string { return s.Metadata.Namespace }

// Key is namespace/name, which names a service in the cluster.
func (s Service) Key() string { return s.Metadata.Namespace + "/" + s.Metadata.Name }

// Selects reports whether the service leads to p: p is in its namespace
// and has every label its selector names. A service with no selector
// selects nothing; its endpoints are set by hand, as the API server's own
// service's are.
func (s Service) Selects(p Pod) bool {
	if len(s.Spec.Selector) == 0 || p.Namespace() != s.Namespace() {
		return false
	}
	for k, v := range s.Spec.Selector {
		if p.Metadata.Labels[k] != v {
			return false
		}
	}
	return true
}

// SelectorText is the selector as kubectl writes one: k=v, by key.
func (s Service) SelectorText() string {
	return labelSelector(s.Spec.Selector)
}

func labelSelector(sel map[string]string) string {
	parts := make([]string, 0, len(sel))
	for _, k := range slices.Sorted(maps.Keys(sel)) {
		parts = append(parts, k+"="+sel[k])
	}
	return strings.Join(parts, ",")
}

// Services lists the services of every namespace.
func (c *Client) Services(ctx context.Context) ([]Service, error) {
	return list[Service](ctx, c, "/api/v1/services")
}

// ContainerPort is a port a container of a pod declares.
type ContainerPort struct {
	Name     string `json:"name,omitempty"`
	Port     int    `json:"containerPort"`
	Protocol string `json:"protocol,omitempty"`
}

// Ports are the ports the pod's containers declare, in their order.
func (p Pod) Ports() []ContainerPort {
	var out []ContainerPort
	for _, c := range p.Spec.Containers {
		out = append(out, c.Ports...)
	}
	return out
}

// Ready reports whether the pod runs and its Ready condition holds.
func (p Pod) Ready() bool {
	return p.Status.Phase == "Running" && p.Metadata.Deleted == nil && condition(p.Status.Conditions, "Ready")
}

// ServicePod picks the pod a connection to port of a service is carried
// to, as kubectl port-forward svc/NAME does: a running, ready pod the
// service's selector matches (the first by name, so the same while it
// lasts), and the port of it the service's targetPort names, by number
// or by the name of a container's port.
func (c *Client) ServicePod(ctx context.Context, namespace, name string, port int) (pod string, podPort int, err error) {
	var svc Service
	if err := c.do(ctx, http.MethodGet, "/api/v1/namespaces/"+url.PathEscape(namespace)+"/services/"+url.PathEscape(name), &svc); err != nil {
		return "", 0, err
	}
	i := slices.IndexFunc(svc.Spec.Ports, func(sp ServicePort) bool { return sp.Port == port })
	if i < 0 {
		return "", 0, fmt.Errorf("service %s has no port %d", name, port)
	}
	if len(svc.Spec.Selector) == 0 {
		return "", 0, fmt.Errorf("service %s has no selector, so no pod to forward to", name)
	}
	q := url.Values{"labelSelector": {labelSelector(svc.Spec.Selector)}}
	pods, err := list[Pod](ctx, c, "/api/v1/namespaces/"+url.PathEscape(namespace)+"/pods?"+q.Encode())
	if err != nil {
		return "", 0, err
	}
	slices.SortFunc(pods, func(a, b Pod) int { return strings.Compare(a.Name(), b.Name()) })
	for _, p := range pods {
		if !p.Ready() {
			continue
		}
		if n, ok := TargetPort(svc.Spec.Ports[i], p); ok {
			return p.Name(), n, nil
		}
	}
	return "", 0, fmt.Errorf("no running pod of service %s is ready", name)
}

// TargetPort is the port of p that a service's port leads to: its
// targetPort's number, the port of p's container that targetPort names,
// or the service's port when it names none.
func TargetPort(sp ServicePort, p Pod) (int, bool) {
	t := sp.TargetPort
	switch {
	case t.Str != "":
		for _, cp := range p.Ports() {
			if cp.Name == t.Str {
				return cp.Port, true
			}
		}
		return 0, false
	case t.Int > 0:
		return t.Int, true
	}
	return sp.Port, true
}
