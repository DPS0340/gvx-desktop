package kubeapi

import (
	"context"
	"net/http"
	"net/url"
)

// Node is a node of the cluster, as far as the app reads one.
type Node struct {
	Metadata Meta `json:"metadata"`
	Status   struct {
		Conditions []Condition `json:"conditions"`
		Addresses  []struct {
			Type    string `json:"type"`
			Address string `json:"address"`
		} `json:"addresses"`
		NodeInfo struct {
			KubeletVersion string `json:"kubeletVersion"`
		} `json:"nodeInfo"`
	} `json:"status"`
}

// Name is the node's name.
func (n Node) Name() string { return n.Metadata.Name }

// Ready reports whether the node's kubelet says it is ready.
func (n Node) Ready() bool { return condition(n.Status.Conditions, "Ready") }

// InternalIP is the address the cluster reaches the node at.
func (n Node) InternalIP() string {
	for _, a := range n.Status.Addresses {
		if a.Type == "InternalIP" {
			return a.Address
		}
	}
	return ""
}

// Nodes lists the cluster's nodes.
func (c *Client) Nodes(ctx context.Context) ([]Node, error) {
	var list struct {
		Items []Node `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/nodes", &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// DeleteNode deletes a node's object, as kubectl delete node does: its
// pods are left to their controllers to make again elsewhere.
func (c *Client) DeleteNode(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/nodes/"+url.PathEscape(name), nil)
}
