package kubeapi

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// kubelet stands in for the kubelet behind the API server's portforward
// subresource: it speaks v4.channel.k8s.io as the kubelet does, carrying
// each websocket to a port of a pod that dial reaches, or saying on the
// error channel why it cannot.
type kubelet struct {
	t    *testing.T
	dial func(pod string, port int) (net.Conn, error)
	mu   sync.Mutex
	open int // websockets being served
	asks []string
}

func (k *kubelet) serve(w http.ResponseWriter, r *http.Request) bool {
	pod, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/namespaces/default/pods/"), "/portforward")
	if !ok {
		return false
	}
	k.mu.Lock()
	k.asks = append(k.asks, r.Method+" "+r.URL.RequestURI()+" "+r.Header.Get("Sec-WebSocket-Protocol"))
	k.mu.Unlock()
	if pod == "gone" {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"kind":"Status","message":"pods \"gone\" not found","reason":"NotFound","code":404}`))
		return true
	}
	if r.Header.Get("Upgrade") != "websocket" || r.Header.Get("Sec-WebSocket-Protocol") != portForwardProtocol {
		w.WriteHeader(http.StatusBadRequest)
		return true
	}
	port, _ := strconv.Atoi(r.URL.Query().Get("ports"))
	conn, brw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		k.t.Error(err)
		return true
	}
	k.mu.Lock()
	k.open++
	k.mu.Unlock()
	defer func() {
		k.mu.Lock()
		k.open--
		k.mu.Unlock()
		conn.Close()
	}()
	sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + wsGUID))
	fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\nSec-WebSocket-Protocol: %s\r\n\r\n",
		base64.StdEncoding.EncodeToString(sum[:]), portForwardProtocol)
	var wmu sync.Mutex
	send := func(op byte, payload []byte, fin bool) {
		wmu.Lock()
		defer wmu.Unlock()
		head := []byte{op, 0}
		if fin {
			head[0] |= 0x80
		}
		switch n := len(payload); {
		case n < 126:
			head[1] = byte(n)
		case n <= 0xffff:
			head[1] = 126
			head = binary.BigEndian.AppendUint16(head, uint16(n))
		default:
			head[1] = 127
			head = binary.BigEndian.AppendUint64(head, uint64(n))
		}
		_, _ = brw.Write(append(head, payload...))
		_ = brw.Flush()
	}
	var lp [2]byte
	binary.LittleEndian.PutUint16(lp[:], uint16(port))
	// The port leads each channel: on the data channel in two messages and
	// a ping between, which the client must take in its stride.
	send(opBinary, []byte{dataChannel, lp[0]}, true)
	send(opPing, []byte("hi"), true)
	send(opBinary, []byte{dataChannel, lp[1]}, true)
	send(opBinary, []byte{errorChannel, lp[0], lp[1]}, true)

	back, err := k.dial(pod, port)
	if err != nil {
		send(opBinary, append([]byte{errorChannel}, err.Error()...), true)
		send(opClose, []byte{0x03, 0xe8}, true)
		return true
	}
	defer back.Close()
	go func() {
		buf := make([]byte, 64*1024)
		for {
			n, err := back.Read(buf)
			if n > 0 {
				// In two fragments, the second a continuation.
				half := 1 + n/2
				msg := append([]byte{dataChannel}, buf[:n]...)
				send(opBinary, msg[:half], false)
				send(opContinue, msg[half:], true)
			}
			if err != nil {
				send(opClose, []byte{0x03, 0xe8}, true)
				conn.Close()
				return
			}
		}
	}()
	ws := &wsConn{conn: conn, br: brw.Reader}
	for {
		fin, op, payload, err := ws.readFrame()
		if err != nil {
			return true
		}
		switch {
		case op == opClose:
			return true
		case op == opPong:
			if string(payload) != "hi" {
				k.t.Errorf("pong %q", payload)
			}
		case op == opBinary && fin && len(payload) > 0 && payload[0] == dataChannel:
			if _, err := back.Write(payload[1:]); err != nil {
				return true
			}
		default:
			k.t.Errorf("frame %#x %v %q", op, fin, payload)
		}
	}
}

// echo is a port that answers each line with it in upper case.
func echo() (net.Conn, error) {
	a, b := net.Pipe()
	go func() {
		defer b.Close()
		sc := bufio.NewScanner(b)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			if _, err := fmt.Fprintln(b, strings.ToUpper(sc.Text())); err != nil {
				return
			}
		}
	}()
	return a, nil
}

func newKubelet(t *testing.T) (*kubelet, *Client) {
	k := &kubelet{t: t, dial: func(pod string, port int) (net.Conn, error) {
		if port != 80 {
			return nil, fmt.Errorf("error forwarding port %d to pod %s, uid : failed to connect to localhost:%d inside namespace: connection refused", port, pod, port)
		}
		return echo()
	}}
	dns, ips := k3sNames()
	cl := newCluster(t, func(w http.ResponseWriter, r *http.Request) {
		if k.serve(w, r) {
			return
		}
		switch {
		case r.URL.Path == "/api/v1/namespaces/default/services/web":
			_, _ = w.Write([]byte(webServiceJSON))
		case r.URL.Path == "/api/v1/namespaces/default/pods":
			_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"web-1","namespace":"default"},"spec":{"containers":[{"name":"web","ports":[{"name":"http","containerPort":80}]}]},
 "status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}, dns, ips)
	return k, connect(t, cl.entry)
}

func TestDialPod(t *testing.T) {
	k, c := newKubelet(t)
	ctx := context.Background()

	pc, err := c.DialPod(ctx, "default", "web-1", 80)
	if err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(pc)
	big := strings.Repeat("x", 100_000) // past a frame's 16-bit length, and maxFrame
	for _, line := range []string{"hello", "port forward", big} {
		if _, err := fmt.Fprintln(pc, line); err != nil {
			t.Fatal(err)
		}
		got, err := r.ReadString('\n')
		if err != nil || got != strings.ToUpper(line)+"\n" {
			t.Fatalf("%.20q: %.20q %v", line, got, err)
		}
	}
	pc.Close()
	if _, err := fmt.Fprintln(pc, "after"); err == nil {
		t.Error("written after Close")
	}
	if pc.Err() != nil {
		t.Errorf("err %v", pc.Err())
	}

	// A port nothing listens on: the kubelet says so on the error channel,
	// and the connection ends.
	pc, err = c.DialPod(ctx, "default", "web-1", 81)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := io.ReadAll(pc); len(b) != 0 || err != nil {
		t.Errorf("read %q, %v", b, err)
	}
	if err := pc.Err(); err == nil || err.Error() != "error forwarding port 81 to pod web-1, uid : failed to connect to localhost:81 inside namespace: connection refused" {
		t.Errorf("err %v", err)
	}
	pc.Close()

	// A pod that is gone is the API server's answer.
	_, err = c.DialPod(ctx, "default", "gone", 80)
	var api *APIError
	if !errors.As(err, &api) || api.Reason != "NotFound" {
		t.Errorf("a pod that is gone: %v", err)
	}
	if _, err := c.DialPod(ctx, "default", "web-1", 0); err == nil {
		t.Error("port 0")
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.asks[0] != "GET /api/v1/namespaces/default/pods/web-1/portforward?ports=80 v4.channel.k8s.io" {
		t.Errorf("asked %s", k.asks[0])
	}
}

func TestForward(t *testing.T) {
	k, c := newKubelet(t)
	for _, target := range []Target{
		{Namespace: "default", Pod: "web-1", Port: 80},
		{Namespace: "default", Service: "web", Port: 80}, // its targetPort http is the pod's 80
	} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		reports := make(chan error, 10)
		done := make(chan error, 1)
		go func() { done <- c.Forward(ctx, ln, target, func(err error) { reports <- err }) }()

		// Two connections at once, each carried on its own.
		var conns []net.Conn
		for i := range 2 {
			conn, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			conns = append(conns, conn)
			fmt.Fprintf(conn, "conn %d\n", i)
		}
		for i, conn := range conns {
			line, err := bufio.NewReader(conn).ReadString('\n')
			if err != nil || line != fmt.Sprintf("CONN %d\n", i) {
				t.Errorf("%+v: %q %v", target, line, err)
			}
		}
		conns[0].Close()
		if err := <-reports; err != nil {
			t.Errorf("%+v: a connection closed: %v", target, err)
		}

		// Stopping ends the connections still open and the listener.
		cancel()
		if err := <-done; err != nil {
			t.Errorf("%+v: Forward: %v", target, err)
		}
		if err := <-reports; err != nil {
			t.Errorf("%+v: the connection stopped: %v", target, err)
		}
		if _, err := conns[1].Read(make([]byte, 1)); err == nil {
			t.Errorf("%+v: a connection outlived the forward", target)
		}
		if c, err := net.Dial("tcp", ln.Addr().String()); err == nil {
			c.Close()
			t.Errorf("%+v: the port still listens", target)
		}
	}
	waitFor(t, func() bool { k.mu.Lock(); defer k.mu.Unlock(); return k.open == 0 }, "the websockets to end")

	// A connection that cannot be carried is reported, and the forward
	// goes on listening.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx := t.Context()
	reports := make(chan error, 10)
	go func() {
		_ = c.Forward(ctx, ln, Target{Namespace: "default", Pod: "gone", Port: 80}, func(err error) { reports <- err })
	}()
	for range 2 {
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		if err := <-reports; err == nil || err.Error() != `pods "gone" not found` {
			t.Errorf("report %v", err)
		}
		if _, err := conn.Read(make([]byte, 1)); err == nil {
			t.Error("the connection stayed open")
		}
		conn.Close()
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
