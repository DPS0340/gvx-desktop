# Kubernetes clusters, as kind makes them

Status: phases 1 (nodes) and 2 (images) implemented; phase 3 is a design.

The Kubernetes recipe runs one k3s server in one sandbox. kind (and k3d,
its k3s twin) make a cluster of several nodes, one container each, several
clusters side by side, load images into them without a registry, and pin
their Kubernetes version. This design brings those to the recipe, in three
phases, each useful alone:

1. **Nodes.** Agents join the recipe's cluster, one sandbox each.
2. **Images.** An image of the Docker Engine (or the sandbox engine) goes
   into every node of the cluster, as `kind load docker-image` does.
3. **Clusters.** Several named clusters, each with its version, ports,
   kubeconfig and kubectl context.

kind itself (its CLI against the app's Docker API) is not a goal: its node
image runs systemd as PID 1, which gVisor does not, and kindnet routes pod
networks between nodes at layer 2.

## 1. Facts this design relies on

Checked on rungv v0.20.1001, darwin/arm64, k3s v1.31.4+k3s1, with a spike
over the sandbox engine (two containers on a user network, so on the
overlay of `internal/overlay`):

- A k3s agent in one sandbox joins a k3s server in another over the
  overlay: `--node-ip=<overlay address> --flannel-iface=gvx0` on both, the
  server's address in `K3S_URL`. Both nodes are `Ready` within 50 s, with
  the overlay addresses as their InternalIPs.
- Pods on different nodes reach each other by pod IP and by service name,
  and `kubectl logs` and `exec` reach a pod on the agent (the API server
  reaches the agent's kubelet on its node IP), with flannel's default
  backend, `vxlan`, and with `wireguard-native` alike.
- containerd's overlayfs snapshotter needs `/var/lib/rancher` on an
  ext4lite volume, as the recipe has it; on a sandbox's root file system
  (itself an overlay) it fails and the node never registers.
- The hub needs no change: the nodes' traffic between them is between
  their overlay addresses, which share one network. Pod addresses travel
  inside flannel's tunnel.

## 2. Phase 1: nodes

### 2.1 Topology

- The cluster's nodes are on the subnet `10.88.0.0/24`, on an overlay
  fabric of the app's own (`overlay.New`), not the sandbox engine's: the
  server is `10.88.0.2`, agent *n* is `10.88.0.(2+n)`. The fabric has one
  network, the cluster's; every node is a member.
- A node's place on the overlay is its `--node-ip` flag: a recipe sandbox
  (server or agent) whose command has `--node-ip=` inside `10.88.0.0/24`
  joins at each start (`recipes.NodeAddr`). No new spec field.
- **Server.** A single-node cluster stays as it is today, off the overlay.
  The first agent added moves the server onto it: `--node-ip=10.88.0.2
  --flannel-iface=gvx0` are added to its command and it restarts (the UI
  says so first). Its node keeps its name; its InternalIP changes.
- **Agent.** A sandbox with recipe `k3s-agent`, named `k3s-agent-<n>`,
  hostname the same (its node name), the server's image, and:
  - command `agent --node-ip=10.88.0.(2+n) --flannel-iface=gvx0
    --private-registry=/output/registries.yaml`;
  - env `IPTABLES_MODE=legacy`, `K3S_URL=https://10.88.0.2:6443`,
    `K3S_TOKEN=<the server's node token>`;
  - the volume `k3s-agent-<n>` at `/var/lib/rancher`, and `~/.gvx/k3s` at
    `/output`, for the registries file (`recipes.Prepare` writes it, as for
    the server).
  - No published ports: NodePorts reach any node's pods through the
    server's, as kube-proxy forwards them.
- **Token.** The server's token cannot be passed in afterwards: a server
  whose datastore exists refuses a token other than its own. The app reads
  `/var/lib/rancher/k3s/server/node-token` from the running server through
  rungv's control (exec, v0.20) when a node is added, and keeps it in the
  agent's spec. Adding nodes needs rungv v0.20. The token goes into the
  agent's run as `-e K3S_TOKEN=…`, which the run's log shows redacted, as
  netinit's keys are.
- **Readiness.** An agent is ready at k3s's "Annotations and labels have
  been set successfully on node" (its node registered).

### 2.2 Flannel backend

The default, `vxlan`, works across nodes (§1), so the recipe sets none and
a cluster that grows keeps its backend. `wireguard-native` works too, at
the cost of a second encryption inside the overlay's.

### 2.3 Starting a node

The supervisor gets a hook, `Launch func(Spec) (run Spec, release func(),
err error)`, called after `Prepare`: rungv runs `run`, the sandbox keeps
its spec, and `release` is called once, as the run ends (before a `Stop`
returns, so a restart's new run is not released by the old one) or when
it fails to start. A failing `Launch` keeps the sandbox from starting.

The app's hook (`internal/nodenet`), for a spec with a node address:

1. joins the fabric (`Fabric.Join(id, [10.88.0.x/24])`) and sets the
   members, each named by its hostname for the fabric's DNS;
2. returns the spec with netinit in front of its command
   (`overlay.WithNetinit`, moved out of `internal/engine`), `-host-dns` and
   `-tune net.nat.mtu` as the sandbox engine does. A node leaves its
   entrypoint to its image; netinit needs it, so it is k3s's, `/bin/k3s`;
3. and its release takes the node off the fabric.

netinit's binary is written to the app's data directory, `nodes/netinit`,
once (`guest.Install`, shared with the engine).

### 2.4 The UI

The Kubernetes page gets a **Nodes** card:

- one row per node: the server first, then the agents; the sandbox's state
  and the node's (`Ready`, from the API: `kubeapi.Client.Nodes`), its
  overlay address, and for an agent, Remove;
- **Add node** (off unless the cluster runs and rungv has control): the
  first one says the server restarts.

Starting, stopping, pausing and resuming the cluster's server (the page's
hero, the sidebar, the tray) does the same to its agents. Removing a node
stops its sandbox, deletes its node object (`kubectl delete node`), its
spec and its volume. Deleting the server's sandbox deletes the agents'
too; their volumes stay, as the server's does.

### 2.5 Verified

- `internal/nodenet`'s `TestHarnessTwoNodes` (`GVX_NODENET_HARNESS=1`,
  real rungv v0.20.1001, a cache and volumes of its own under
  `/tmp/gvxd-nodenet`): a server runs alone, moves onto the network, an
  agent joins with its token, both nodes are `Ready`, a pod on each runs,
  `kubectl logs` reaches the agent's, and the server's pod reaches the
  agent's by pod IP. About 50 s.
- `internal/ui`'s `TestAddNode`, with a stand-in rungv: the first node
  asks, restarts the server onto the network and starts an agent with the
  server's token, both running netinit; the second does not ask; the
  cluster stops and starts whole; a node is removed alone; deleting the
  server deletes the cluster.

## 3. Phase 2: images

**Load into cluster**, in a Docker Engine image's menu (Images → Docker
Engine), puts the image into every running node of the cluster:

- the app saves it from the engine (`GET /images/{name}/get`,
  `dockerapi.Client.SaveImage`, as `docker save` does) to a temporary file;
- and pipes the file into `ctr -n k8s.io images import -` on each node
  through rungv's exec with standard input (`nodenet.ImportImage`), one
  node after another. k3s's `ctr` finds its containerd by itself.

Pods then run it by its name without pulling it, while their pull policy
lets them: `IfNotPresent`, the default for a tag other than `latest`. A
node that is not running, or one added later, does not get it: load it
again, as with kind. The image needs a tag, and this computer's CPU. It
needs rungv v0.20 (exec).

Verified in `TestHarnessTwoNodes`: `alpine:3.20`, saved by rungv, goes into
both nodes, and a pod on the agent runs it with `imagePullPolicy: Never`.
`internal/ui`'s `TestLoadIntoCluster` checks the model's side with a stand-in
engine and nodes.

## 3.1 Navigation

Kubernetes leaves the Recipes section of the sidebar for an entry of its
own, the cluster's page, with Pods, Workloads and Services under it while
the cluster runs (the Pods page's views; its tabs stay, for a sidebar that
is hidden).

## 4. Phase 3: clusters (design)

The recipe's single-cluster assumptions (`recipes.Kubeconfig`, the context
`gvx-k3s`, ports 6443 and 30000-30019, the volume `k3s`,
`RecipeSandbox(rows, k3s)`) become a cluster's: a name, a k3s version, an
API port (6443 for the first, the next free one after), a NodePort range,
`~/.gvx/k3s/<name>/kubeconfig.yaml` and the context `gvx-<name>`. The Pods
pages get a cluster picker.
