package kubeapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

const servicesJSON = `{"kind":"ServiceList","apiVersion":"v1","items":[
{"metadata":{"name":"kubernetes","namespace":"default","creationTimestamp":"2026-11-13T04:00:00Z"},
 "spec":{"type":"ClusterIP","clusterIP":"10.43.0.1","ports":[{"name":"https","protocol":"TCP","port":443,"targetPort":6443}]},"status":{"loadBalancer":{}}},
{"metadata":{"name":"web","namespace":"default"},
 "spec":{"type":"NodePort","clusterIP":"10.43.12.7","selector":{"app":"web","tier":"front"},
  "ports":[{"protocol":"TCP","port":80,"targetPort":"http","nodePort":30000},{"name":"metrics","protocol":"TCP","port":9113,"targetPort":9113,"nodePort":30080}]}},
{"metadata":{"name":"lb","namespace":"kube-system"},
 "spec":{"type":"LoadBalancer","clusterIP":"10.43.1.1","selector":{"app":"lb"},"ports":[{"port":443,"nodePort":31443}]},
 "status":{"loadBalancer":{"ingress":[{"ip":"10.0.2.15","ipMode":"VIP"}]}}}]}`

func TestServices(t *testing.T) {
	dns, ips := k3sNames()
	var calls []request
	k := newCluster(t, recorder(&calls, func(r *http.Request) (int, string) {
		if r.URL.Path == "/api/v1/services" {
			return 200, servicesJSON
		}
		return 500, ""
	}), dns, ips)
	list, err := connect(t, k.entry).Services(context.Background())
	if err != nil || len(list) != 3 {
		t.Fatalf("%d services, %v", len(list), err)
	}
	api, web, lb := list[0], list[1], list[2]
	if api.Key() != "default/kubernetes" || api.Spec.Type != "ClusterIP" || api.Spec.ClusterIP != "10.43.0.1" ||
		api.Spec.Ports[0].TargetPort != (IntOrString{Int: 6443}) || api.Metadata.Created.IsZero() {
		t.Errorf("kubernetes %+v", api)
	}
	p := web.Spec.Ports
	if p[0].TargetPort != (IntOrString{Str: "http"}) || p[0].TargetPort.String() != "http" || p[0].NodePort != 30000 ||
		p[1].Name != "metrics" || p[1].TargetPort.String() != "9113" || p[1].NodePort != 30080 {
		t.Errorf("web's ports %+v", p)
	}
	if web.SelectorText() != "app=web,tier=front" || api.SelectorText() != "" {
		t.Errorf("selectors %q %q", web.SelectorText(), api.SelectorText())
	}
	if lb.Status.LoadBalancer.Ingress[0].IP != "10.0.2.15" || p[0].TargetPort == (IntOrString{}) {
		t.Errorf("lb %+v", lb)
	}
	if b, _ := json.Marshal(p); string(b) != `[{"protocol":"TCP","port":80,"targetPort":"http","nodePort":30000},{"name":"metrics","protocol":"TCP","port":9113,"targetPort":9113,"nodePort":30080}]` {
		t.Errorf("ports written back: %s", b)
	}

	pod := func(ns string, labels map[string]string) Pod {
		return Pod{Metadata: Meta{Namespace: ns, Name: "p", Labels: labels}}
	}
	for _, c := range []struct {
		s    Service
		p    Pod
		want bool
	}{
		{web, pod("default", map[string]string{"app": "web", "tier": "front", "pod-template-hash": "1"}), true},
		{web, pod("default", map[string]string{"app": "web"}), false},
		{web, pod("other", map[string]string{"app": "web", "tier": "front"}), false},
		{api, pod("default", map[string]string{}), false}, // no selector: none
	} {
		if got := c.s.Selects(c.p); got != c.want {
			t.Errorf("%s selects %v: %v", c.s.Name(), c.p.Metadata.Labels, got)
		}
	}
}

// podsOfWeb are the pods the web service selects: one terminating, one not
// ready, two ready, which name their port http.
const podsOfWeb = `{"items":[
{"metadata":{"name":"web-3","namespace":"default","deletionTimestamp":"2026-11-16T06:00:00Z"},"spec":{"containers":[{"name":"web","ports":[{"name":"http","containerPort":8080}]}]},
 "status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}},
{"metadata":{"name":"web-2","namespace":"default"},"spec":{"containers":[{"name":"web","ports":[{"name":"http","containerPort":8080}]}]},
 "status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}},
{"metadata":{"name":"web-0","namespace":"default"},"spec":{"containers":[{"name":"web","ports":[{"name":"http","containerPort":8080}]}]},
 "status":{"phase":"Running","conditions":[{"type":"Ready","status":"False"}]}},
{"metadata":{"name":"web-1","namespace":"default"},"spec":{"containers":[{"name":"sidecar"},{"name":"web","ports":[{"name":"metrics","containerPort":9113},{"name":"http","containerPort":8080}]}]},
 "status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}]}`

func TestServicePod(t *testing.T) {
	dns, ips := k3sNames()
	var calls []request
	k := newCluster(t, recorder(&calls, func(r *http.Request) (int, string) {
		switch r.URL.Path {
		case "/api/v1/namespaces/default/services/web":
			return 200, webServiceJSON
		case "/api/v1/namespaces/default/services/kubernetes":
			return 200, `{"metadata":{"name":"kubernetes","namespace":"default"},"spec":{"ports":[{"port":443,"targetPort":6443}]}}`
		case "/api/v1/namespaces/default/services/nobody":
			return 200, `{"metadata":{"name":"nobody","namespace":"default"},"spec":{"selector":{"app":"none"},"ports":[{"port":80}]}}`
		case "/api/v1/namespaces/default/pods":
			if r.URL.Query().Get("labelSelector") == "app=web,tier=front" {
				return 200, podsOfWeb
			}
			return 200, `{"items":[]}`
		}
		return 404, `{"kind":"Status","message":"services \"x\" not found","reason":"NotFound","code":404}`
	}), dns, ips)
	c := connect(t, k.entry)
	ctx := context.Background()

	// The first ready pod by name, its port named http.
	pod, port, err := c.ServicePod(ctx, "default", "web", 80)
	if err != nil || pod != "web-1" || port != 8080 {
		t.Errorf("port 80: %s %d %v", pod, port, err)
	}
	// A targetPort by number.
	if pod, port, err = c.ServicePod(ctx, "default", "web", 9113); err != nil || pod != "web-1" || port != 9113 {
		t.Errorf("port 9113: %s %d %v", pod, port, err)
	}
	for _, x := range []struct {
		name string
		port int
		want string
	}{
		{"web", 81, "service web has no port 81"},
		{"kubernetes", 443, "service kubernetes has no selector, so no pod to forward to"},
		{"nobody", 80, "no running pod of service nobody is ready"},
		{"missing", 80, `services "x" not found`},
	} {
		if _, _, err := c.ServicePod(ctx, "default", x.name, x.port); err == nil || err.Error() != x.want {
			t.Errorf("%s:%d: %v", x.name, x.port, err)
		}
	}
	if calls[1].uri != "/api/v1/namespaces/default/pods?labelSelector=app%3Dweb%2Ctier%3Dfront" {
		t.Errorf("the pods were asked for with %s", calls[1].uri)
	}

	// A service's port with no targetPort leads to the same port of the pod.
	if n, ok := TargetPort(ServicePort{Port: 80}, Pod{}); !ok || n != 80 {
		t.Errorf("no targetPort: %d %v", n, ok)
	}
	if _, ok := TargetPort(ServicePort{Port: 80, TargetPort: IntOrString{Str: "grpc"}}, Pod{}); ok {
		t.Error("a name no container port has")
	}
}

const webServiceJSON = `{"metadata":{"name":"web","namespace":"default"},
 "spec":{"type":"NodePort","clusterIP":"10.43.12.7","selector":{"app":"web","tier":"front"},
  "ports":[{"protocol":"TCP","port":80,"targetPort":"http","nodePort":30000},{"name":"metrics","protocol":"TCP","port":9113,"targetPort":9113,"nodePort":30080}]}}`
