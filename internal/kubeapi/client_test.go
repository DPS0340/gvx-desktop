package kubeapi

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// authority is a CA made for a test, as k3s makes its own.
type authority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

var serial int64

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func newAuthority(t *testing.T, name string) authority {
	t.Helper()
	key := newKey(t)
	serial++
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return authority{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue signs a certificate for a server (with names) or a client.
func (a authority) issue(t *testing.T, cn string, client bool, dns []string, ips []net.IP) (certPEM, keyPEM []byte) {
	t.Helper()
	key := newKey(t)
	serial++
	usage := x509.ExtKeyUsageServerAuth
	if client {
		usage = x509.ExtKeyUsageClientAuth
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: cn, Organization: []string{"system:masters"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		DNSNames:     dns,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, &key.PublicKey, a.key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
}

// cluster is an API server over TLS as k3s serves one: its certificate
// names localhost, kubernetes and 127.0.0.1 unless names says otherwise,
// and it asks clients for a certificate of its CA, or a bearer token.
type cluster struct {
	ca    authority
	srv   *httptest.Server
	entry Entry // a kubeconfig's entry that reaches it with a certificate
}

func newCluster(t *testing.T, handler http.HandlerFunc, dns []string, ips []net.IP) *cluster {
	t.Helper()
	ca := newAuthority(t, "k3s-server-ca@1731744000")
	srvCert, srvKey := ca.issue(t, "k3s", false, dns, ips)
	pair, err := tls.X509KeyPair(srvCert, srvKey)
	if err != nil {
		t.Fatal(err)
	}
	clients := x509.NewCertPool()
	clients.AddCert(ca.cert)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.PeerCertificates) == 0 && r.Header.Get("Authorization") != "Bearer secret-token" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"Unauthorized","reason":"Unauthorized","code":401}`))
			return
		}
		handler(w, r)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, ClientCAs: clients, ClientAuth: tls.VerifyClientCertIfGiven}
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the handshakes the tests make fail
	srv.StartTLS()
	t.Cleanup(srv.Close)
	cert, key := ca.issue(t, "system:admin", true, nil, nil)
	b64 := base64.StdEncoding.EncodeToString
	return &cluster{ca: ca, srv: srv, entry: Entry{Server: srv.URL, CA: b64(ca.pem), Cert: b64(cert), Key: b64(key)}}
}

func k3sNames() ([]string, []net.IP) {
	return []string{"kubernetes", "kubernetes.default", "kubernetes.default.svc", "kubernetes.default.svc.cluster.local", "localhost", "k3s"},
		[]net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1"), net.ParseIP("10.43.0.1")}
}

func connect(t *testing.T, e Entry) *Client {
	t.Helper()
	c, err := New(e)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestTLSVerifiesTheServerAgainstTheKubeconfigCA(t *testing.T) {
	dns, ips := k3sNames()
	k := newCluster(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"kind":"PodList","items":[]}`))
	}, dns, ips)
	ctx := context.Background()

	// The server is https://127.0.0.1:port, which its certificate names.
	if !strings.HasPrefix(k.entry.Server, "https://127.0.0.1:") {
		t.Fatalf("server %s", k.entry.Server)
	}
	if _, err := connect(t, k.entry).Pods(ctx); err != nil {
		t.Fatalf("with the cluster's CA: %v", err)
	}

	// Another CA: the server is not trusted.
	other := newAuthority(t, "someone-else")
	wrong := k.entry
	wrong.CA = base64.StdEncoding.EncodeToString(other.pem)
	_, err := connect(t, wrong).Pods(ctx)
	var unknown x509.UnknownAuthorityError
	if !errors.As(err, &unknown) {
		t.Errorf("with another CA: %v", err)
	}

	// No CA: the system's roots do not know the cluster's.
	none := k.entry
	none.CA = ""
	if _, err := connect(t, none).Pods(ctx); err == nil {
		t.Error("with no CA, the server was trusted")
	}

	// The same server, reached by a name its certificate does not hold.
	misnamed := k.entry
	misnamed.ServerName = "elsewhere.example"
	var host x509.HostnameError
	if _, err := connect(t, misnamed).Pods(ctx); !errors.As(err, &host) {
		t.Errorf("by a name it does not hold: %v", err)
	}

	// localhost is named too.
	byName := k.entry
	byName.Server = strings.Replace(byName.Server, "127.0.0.1", "localhost", 1)
	if _, err := connect(t, byName).Pods(ctx); err != nil {
		t.Errorf("by localhost: %v", err)
	}
}

// A certificate that does not name the address the server is reached at
// is accepted for the name tls-server-name gives, and only for it.
func TestTLSServerName(t *testing.T) {
	k := newCluster(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":[]}`))
	}, []string{"kubernetes"}, nil)
	ctx := context.Background()
	if _, err := connect(t, k.entry).Pods(ctx); err == nil {
		t.Error("an IP address the certificate does not name was trusted")
	}
	named := k.entry
	named.ServerName = "kubernetes"
	if _, err := connect(t, named).Pods(ctx); err != nil {
		t.Errorf("with tls-server-name: %v", err)
	}
}

func TestTokenAndUnauthorized(t *testing.T) {
	dns, ips := k3sNames()
	k := newCluster(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":[]}`))
	}, dns, ips)
	ctx := context.Background()
	tok := Entry{Server: k.entry.Server, CA: k.entry.CA, Token: "secret-token"}
	if _, err := connect(t, tok).Pods(ctx); err != nil {
		t.Errorf("with a token: %v", err)
	}
	tok.Token = "wrong"
	_, err := connect(t, tok).Pods(ctx)
	var api *APIError
	if !errors.As(err, &api) || api.Code != 401 || api.Message != "Unauthorized" {
		t.Errorf("with a wrong token: %#v", err)
	}
}

func TestNewRefusesBrokenEntries(t *testing.T) {
	for name, e := range map[string]Entry{
		"http":        {Server: "http://127.0.0.1:6443"},
		"no host":     {Server: "https://"},
		"bad url":     {Server: "https://[::1"},
		"bad base64":  {Server: "https://127.0.0.1:6443", CA: "%%%"},
		"not a cert":  {Server: "https://127.0.0.1:6443", CA: base64.StdEncoding.EncodeToString([]byte("nope"))},
		"no key":      {Server: "https://127.0.0.1:6443", Cert: base64.StdEncoding.EncodeToString([]byte("nope"))},
		"bad cert 64": {Server: "https://127.0.0.1:6443", Cert: "%%%", Key: "a"},
	} {
		if _, err := New(e); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestOpenReadsTheFile(t *testing.T) {
	dns, ips := k3sNames()
	k := newCluster(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"web","namespace":"default"}}]}`))
	}, dns, ips)
	file := filepath.Join(t.TempDir(), "kubeconfig.yaml")
	cfg := strings.Join([]string{
		"apiVersion: v1",
		"clusters:",
		"- cluster:",
		"    certificate-authority-data: " + k.entry.CA,
		"    server: " + k.entry.Server,
		"  name: default",
		"contexts:",
		"- context:",
		"    cluster: default",
		"    user: default",
		"  name: default",
		"current-context: default",
		"kind: Config",
		"preferences: {}",
		"users:",
		"- name: default",
		"  user:",
		"    client-certificate-data: " + k.entry.Cert,
		"    client-key-data: " + k.entry.Key,
		"",
	}, "\n")
	if err := os.WriteFile(file, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	pods, err := c.Pods(context.Background())
	if err != nil || len(pods) != 1 || pods[0].Key() != "default/web" {
		t.Errorf("pods %+v, %v", pods, err)
	}
	if _, err := Open(filepath.Join(t.TempDir(), "none")); !os.IsNotExist(err) {
		t.Errorf("no file: %v", err)
	}
	if err := os.WriteFile(file, []byte("kind: Config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(file); err == nil {
		t.Error("no context, no error")
	}
}

func TestRequests(t *testing.T) {
	dns, ips := k3sNames()
	var calls []string
	k := newCluster(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/pods":
			_, _ = w.Write([]byte(podListJSON))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/namespaces/default/pods/gone":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"pods \"gone\" not found","reason":"NotFound","details":{"name":"gone","kind":"pods"},"code":404}`))
		case r.Method == http.MethodDelete:
			_, _ = w.Write([]byte(`{"kind":"Pod","apiVersion":"v1","metadata":{"name":"web-7d4b9c8f6d-x7k2p"}}`))
		case strings.HasSuffix(r.URL.Path, "/log"):
			if r.URL.Query().Get("container") == "waiting" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"kind":"Status","status":"Failure","message":"container \"waiting\" in pod \"web\" is waiting to start: ContainerCreating","reason":"BadRequest","code":400}`))
				return
			}
			_, _ = w.Write([]byte("2026-11-16T06:24:33.123456789Z ready\n2026-11-16T06:24:34Z \n2026-11-16T06:24:35.5Z GET / 200\r\nno stamp\n"))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}, dns, ips)
	c := connect(t, k.entry)
	ctx := context.Background()

	pods, err := c.Pods(ctx)
	if err != nil || len(pods) != 2 {
		t.Fatalf("pods %d, %v", len(pods), err)
	}
	p := pods[0]
	if p.Key() != "default/web-7d4b9c8f6d-x7k2p" || p.Spec.Node != "k3s" || p.Status.PodIP != "10.42.0.12" ||
		!p.Metadata.Created.Equal(time.Date(2026, 11, 16, 4, 0, 0, 0, time.UTC)) || p.Metadata.Deleted != nil {
		t.Errorf("pod %+v", p)
	}
	if w, ok := p.Workload(); !ok || w.Kind != "Deployment" || w.Name != "web" {
		t.Errorf("workload %+v", w)
	}
	if s := p.Summarize(); s.Status != "Running" || s.ReadyText() != "2/2" || s.Restarts != 1 || s.Health() != Healthy ||
		!s.LastRestart.Equal(time.Date(2026, 11, 16, 5, 0, 0, 0, time.UTC)) {
		t.Errorf("summary %+v", s)
	}
	if names := strings.Join(p.ContainerNames(), ","); names != "web,sidecar,init-db" {
		t.Errorf("containers %s", names)
	}
	if pods[1].Metadata.Deleted == nil || pods[1].Summarize().Status != "Terminating" {
		t.Errorf("the deleted pod %+v", pods[1].Summarize())
	}

	if err := c.DeletePod(ctx, "default", "web-7d4b9c8f6d-x7k2p"); err != nil {
		t.Error(err)
	}
	err = c.DeletePod(ctx, "default", "gone")
	var api *APIError
	if !errors.As(err, &api) || api.Reason != "NotFound" || err.Error() != `pods "gone" not found` {
		t.Errorf("deleting a pod that is gone: %v", err)
	}

	lines, err := c.Logs(ctx, "default", "web", "web", 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 4 || lines[0].Text != "ready" || lines[0].Time.Nanosecond() != 123456789 ||
		lines[1].Text != "" || lines[1].Time.IsZero() || lines[2].Text != "GET / 200" || lines[3].Text != "no stamp" || !lines[3].Time.IsZero() {
		t.Errorf("lines %+v", lines)
	}
	_, err = c.Logs(ctx, "default", "web", "waiting", 500)
	if err == nil || !strings.Contains(err.Error(), "is waiting to start: ContainerCreating") {
		t.Errorf("logs of a waiting container: %v", err)
	}

	want := []string{
		"GET /api/v1/pods",
		"DELETE /api/v1/namespaces/default/pods/web-7d4b9c8f6d-x7k2p",
		"DELETE /api/v1/namespaces/default/pods/gone",
		"GET /api/v1/namespaces/default/pods/web/log?container=web&tailLines=500&timestamps=true",
		"GET /api/v1/namespaces/default/pods/web/log?container=waiting&tailLines=500&timestamps=true",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests\n%s\nwant\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
}

// podListJSON is a GET /api/v1/pods answer cut down to two pods: one of a
// Deployment, with a sidecar and an init container, and one being deleted.
const podListJSON = `{"kind":"PodList","apiVersion":"v1","metadata":{"resourceVersion":"1234"},"items":[
{"metadata":{"name":"web-7d4b9c8f6d-x7k2p","generateName":"web-7d4b9c8f6d-","namespace":"default","uid":"6b1f","resourceVersion":"1200",
  "creationTimestamp":"2026-11-16T04:00:00Z","labels":{"app":"web","pod-template-hash":"7d4b9c8f6d"},
  "ownerReferences":[{"apiVersion":"apps/v1","kind":"ReplicaSet","name":"web-7d4b9c8f6d","uid":"aa","controller":true,"blockOwnerDeletion":true}]},
 "spec":{"nodeName":"k3s","containers":[{"name":"web","image":"nginx:alpine","ports":[{"containerPort":80,"protocol":"TCP"}]},{"name":"sidecar","image":"busybox"}],
  "initContainers":[{"name":"init-db","image":"busybox","command":["sh","-c","true"]}],"restartPolicy":"Always"},
 "status":{"phase":"Running","podIP":"10.42.0.12","podIPs":[{"ip":"10.42.0.12"}],"hostIP":"10.0.2.15","qosClass":"BestEffort","startTime":"2026-11-16T04:00:00Z",
  "conditions":[{"type":"Initialized","status":"True"},{"type":"Ready","status":"True"},{"type":"PodScheduled","status":"True"}],
  "initContainerStatuses":[{"name":"init-db","image":"busybox:latest","ready":true,"restartCount":0,"started":false,
    "state":{"terminated":{"exitCode":0,"reason":"Completed","startedAt":"2026-11-16T04:00:01Z","finishedAt":"2026-11-16T04:00:02Z"}},"lastState":{}}],
  "containerStatuses":[
   {"name":"sidecar","image":"busybox:latest","ready":true,"restartCount":0,"started":true,"state":{"running":{"startedAt":"2026-11-16T04:00:03Z"}},"lastState":{}},
   {"name":"web","image":"nginx:alpine","ready":true,"restartCount":1,"started":true,"state":{"running":{"startedAt":"2026-11-16T05:00:01Z"}},
    "lastState":{"terminated":{"exitCode":137,"reason":"OOMKilled","startedAt":"2026-11-16T04:00:03Z","finishedAt":"2026-11-16T05:00:00Z"}}}]}},
{"metadata":{"name":"old","namespace":"default","uid":"7c","creationTimestamp":"2026-11-15T04:00:00Z","deletionTimestamp":"2026-11-16T06:00:00Z","deletionGracePeriodSeconds":30},
 "spec":{"containers":[{"name":"old","image":"busybox"}]},
 "status":{"phase":"Running","containerStatuses":[{"name":"old","ready":true,"restartCount":0,"state":{"running":{"startedAt":"2026-11-15T04:00:01Z"}},"lastState":{}}]}}
]}`
