package kubeapi

import (
	"context"
	"net/http"
	"testing"
)

func TestNodes(t *testing.T) {
	var deleted string
	dns, ips := k3sNames()
	k := newCluster(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
			_, _ = w.Write([]byte(`{"kind":"NodeList","items":[
				{"metadata":{"name":"k3s"},"status":{"conditions":[{"type":"Ready","status":"True"}],
				 "addresses":[{"type":"InternalIP","address":"10.88.0.2"},{"type":"Hostname","address":"k3s"}],
				 "nodeInfo":{"kubeletVersion":"v1.31.4+k3s1"}}},
				{"metadata":{"name":"k3s-agent-1"},"status":{"conditions":[{"type":"Ready","status":"Unknown"}]}}]}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/nodes/k3s-agent-1":
			deleted = "k3s-agent-1"
			_, _ = w.Write([]byte(`{"kind":"Status","status":"Success"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}, dns, ips)
	c := connect(t, k.entry)
	nodes, err := c.Nodes(context.Background())
	if err != nil || len(nodes) != 2 {
		t.Fatalf("Nodes = %v, %v", nodes, err)
	}
	if n := nodes[0]; n.Name() != "k3s" || !n.Ready() || n.InternalIP() != "10.88.0.2" || n.Status.NodeInfo.KubeletVersion != "v1.31.4+k3s1" {
		t.Errorf("server node %+v", n)
	}
	if n := nodes[1]; n.Ready() || n.InternalIP() != "" {
		t.Errorf("agent node %+v", n)
	}
	if err := c.DeleteNode(context.Background(), "k3s-agent-1"); err != nil || deleted != "k3s-agent-1" {
		t.Errorf("DeleteNode = %v, deleted %q", err, deleted)
	}
}
