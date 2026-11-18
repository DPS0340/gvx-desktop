package kubeapi

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
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
	"time"
)

// Port forwarding, as kubectl port-forward does it, without kubectl: each
// connection to a local port is carried to a port of a pod over a
// connection of its own to the API server, which hands it on to the pod's
// kubelet. The connection speaks the websocket form of the pod's
// portforward subresource, the subprotocol v4.channel.k8s.io: each message
// leads with the number of its channel, 0 for the port's data and 1 for
// the errors of forwarding it, and the first two bytes of each channel are
// the port, little-endian. The API server hands a websocket of this
// subprotocol on to the kubelet untouched; kubectl's own websockets
// (portforward.k8s.io v2) tunnel SPDY instead, which this does not speak.

// portForwardProtocol is the websocket subprotocol DialPod speaks.
const portForwardProtocol = "v4.channel.k8s.io"

// The channels of a forwarded port.
const (
	dataChannel  = 0
	errorChannel = 1
)

// PortConn is a connection to a port of a pod. Read and Write carry the
// port's bytes; Err tells what the kubelet said went wrong, if anything,
// once Read has ended.
type PortConn struct {
	ws   *wsConn
	r    *io.PipeReader
	w    *io.PipeWriter
	mu   sync.Mutex
	err  error // what the error channel said
	once sync.Once
}

// DialPod connects to port of a pod through the API server.
func (c *Client) DialPod(ctx context.Context, namespace, name string, port int) (*PortConn, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("kubeapi: port %d", port)
	}
	ws, err := c.dialWebsocket(ctx, podPath(namespace, name)+"/portforward?ports="+strconv.Itoa(port), portForwardProtocol)
	if err != nil {
		return nil, err
	}
	pc := &PortConn{ws: ws}
	pc.r, pc.w = io.Pipe()
	go pc.read()
	return pc, nil
}

// read hands on what the data channel carries, and keeps what the error
// channel says, until the websocket ends.
func (pc *PortConn) read() {
	var seen [2]int // bytes of each channel read, the port's two among them
	var said strings.Builder
	end := func(err error) {
		if said.Len() > 0 {
			pc.mu.Lock()
			pc.err = errors.New(strings.TrimSpace(said.String()))
			pc.mu.Unlock()
		}
		pc.w.CloseWithError(err)
	}
	for {
		msg, err := pc.ws.readMessage()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				err = nil
			}
			end(err)
			return
		}
		if len(msg) == 0 {
			continue
		}
		ch, data := int(msg[0]), msg[1:]
		if ch != dataChannel && ch != errorChannel {
			continue
		}
		if skip := min(2-seen[ch], len(data)); skip > 0 {
			seen[ch] += skip
			data = data[skip:]
		}
		switch ch {
		case dataChannel:
			if _, err := pc.w.Write(data); err != nil {
				return // closed
			}
		case errorChannel:
			said.Write(data)
		}
	}
}

// Read reads what the pod's port sends.
func (pc *PortConn) Read(p []byte) (int, error) { return pc.r.Read(p) }

// maxFrame is the most a message written carries: the kubelet reads any
// size, and smaller frames let other writes in.
const maxFrame = 32 * 1024

// Write sends p to the pod's port.
func (pc *PortConn) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		chunk := p[:min(len(p), maxFrame)]
		if err := pc.ws.writeData(dataChannel, chunk); err != nil {
			return n, err
		}
		n += len(chunk)
		p = p[len(chunk):]
	}
	return n, nil
}

// Err is what the kubelet said went wrong forwarding the port, such as a
// pod that has nothing listening on it, or nil.
func (pc *PortConn) Err() error {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	return pc.err
}

// Close ends the connection.
func (pc *PortConn) Close() error {
	pc.once.Do(func() {
		pc.ws.close()
		pc.r.Close()
	})
	return nil
}

// ---- Forwarding ---------------------------------------------------------

// Target is what a port forward reaches: a port of a pod, or a port of a
// service, which one of its pods serves.
type Target struct {
	Namespace string
	Pod       string // the pod, or
	Service   string // the service
	Port      int    // the pod's port, or the service's
}

// Forward carries each connection ln accepts to the target, until ctx ends
// or ln fails, and then closes ln and the connections it carries. report
// is told, from any goroutine, how each connection went: nil when it was
// carried to its end, else why it could not be. A service is resolved to
// one of its pods anew for each connection.
func (c *Client) Forward(ctx context.Context, ln net.Listener, t Target, report func(error)) error {
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := ln.Accept()
		if err != nil {
			ln.Close()
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Go(func() { report(c.carry(ctx, conn, t)) })
	}
}

// carry copies between a local connection and the target's port until
// either side ends.
func (c *Client) carry(ctx context.Context, local net.Conn, t Target) error {
	defer local.Close()
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	pod, port := t.Pod, t.Port
	if t.Service != "" {
		var err error
		if pod, port, err = c.ServicePod(dctx, t.Namespace, t.Service, t.Port); err != nil {
			return err
		}
	}
	remote, err := c.DialPod(dctx, t.Namespace, pod, port)
	if err != nil {
		return err
	}
	defer remote.Close()
	stop := context.AfterFunc(ctx, func() { local.Close(); remote.Close() })
	defer stop()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(remote, local); done <- struct{}{} }()
	go func() { _, _ = io.Copy(local, remote); done <- struct{}{} }()
	<-done
	// Either side ending ends both: the protocol has no way to tell the
	// pod that the local side is done sending and waits for an answer.
	local.Close()
	remote.Close()
	<-done
	return remote.Err()
}

// ---- A websocket client -------------------------------------------------

// wsConn is the client's end of a websocket (RFC 6455): enough of it for
// the portforward subresource, which sends binary messages.
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
	wmu  sync.Mutex // one frame at a time
	once sync.Once
}

// wsGUID is what the server's accept key is made with.
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// dialWebsocket opens a websocket to path, over TLS of its own speaking
// HTTP/1.1, which a websocket upgrades, with the client's credentials.
func (c *Client) dialWebsocket(ctx context.Context, path, protocol string) (*wsConn, error) {
	conf := c.tls.Clone()
	conf.NextProtos = []string{"http/1.1"}
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: conf}
	conn, err := d.DialContext(ctx, "tcp", c.host)
	if err != nil {
		return nil, err
	}
	// The handshake ends when ctx does.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	ws, err := c.handshake(conn, path, protocol)
	if !stop() || err != nil {
		conn.Close()
		if err == nil || ctx.Err() != nil {
			err = errors.Join(ctx.Err(), err)
		}
		return nil, err
	}
	return ws, nil
}

func (c *Client) handshake(conn net.Conn, path, protocol string) (*wsConn, error) {
	var nonce [16]byte
	_, _ = rand.Read(nonce[:])
	key := base64.StdEncoding.EncodeToString(nonce[:])
	req, err := http.NewRequest(http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", key)
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Protocol", protocol)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if err := req.Write(conn); err != nil {
		return nil, err
	}
	br := bufio.NewReader(conn)
	res, err := http.ReadResponse(br, req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusSwitchingProtocols {
		defer res.Body.Close()
		return nil, failure(res, http.MethodGet, path)
	}
	sum := sha1.Sum([]byte(key + wsGUID))
	if res.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) {
		return nil, errors.New("kubeapi: the API server's websocket answer does not match its key")
	}
	if got := res.Header.Get("Sec-WebSocket-Protocol"); got != protocol {
		return nil, fmt.Errorf("kubeapi: the API server chose the websocket subprotocol %q, not %s", got, protocol)
	}
	return &wsConn{conn: conn, br: br}, nil
}

// The websocket opcodes.
const (
	opContinue = 0x0
	opText     = 0x1
	opBinary   = 0x2
	opClose    = 0x8
	opPing     = 0x9
	opPong     = 0xa
)

// maxMessage is the most a message read may hold.
const maxMessage = 16 << 20

// readMessage reads the next data message, its fragments joined, answering
// pings on the way. A close from the server is io.EOF.
func (w *wsConn) readMessage() ([]byte, error) {
	var msg []byte
	started := false
	for {
		fin, op, payload, err := w.readFrame()
		if err != nil {
			return nil, err
		}
		switch op {
		case opPing:
			if err := w.writeFrame(opPong, payload); err != nil {
				return nil, err
			}
			continue
		case opPong:
			continue
		case opClose:
			w.close()
			return nil, io.EOF
		case opText, opBinary:
			if started {
				return nil, errors.New("kubeapi: websocket: a message began inside another")
			}
			started, msg = true, payload
		case opContinue:
			if !started {
				return nil, errors.New("kubeapi: websocket: a continuation with no message")
			}
			if len(msg)+len(payload) > maxMessage {
				return nil, errors.New("kubeapi: websocket: message too large")
			}
			msg = append(msg, payload...)
		default:
			return nil, fmt.Errorf("kubeapi: websocket: opcode %#x", op)
		}
		if fin {
			return msg, nil
		}
	}
}

// readFrame reads a frame: whether it ends its message, its opcode and its
// payload, unmasked.
func (w *wsConn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var head [2]byte
	if _, err = io.ReadFull(w.br, head[:]); err != nil {
		return
	}
	fin, op = head[0]&0x80 != 0, head[0]&0x0f
	masked := head[1]&0x80 != 0
	n := uint64(head[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(w.br, b[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(w.br, b[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if n > maxMessage {
		return false, 0, nil, errors.New("kubeapi: websocket: frame too large")
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(w.br, mask[:]); err != nil {
			return
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(w.br, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return fin, op, payload, nil
}

// writeData sends data on a channel, as one binary message.
func (w *wsConn) writeData(ch byte, data []byte) error {
	msg := make([]byte, 1+len(data))
	msg[0] = ch
	copy(msg[1:], data)
	return w.writeFrame(opBinary, msg)
}

// writeFrame sends one whole frame, masked as a client's must be, giving
// up on a peer that takes none of it for half a minute.
func (w *wsConn) writeFrame(op byte, payload []byte) error {
	return w.write(op, payload, 30*time.Second)
}

func (w *wsConn) write(op byte, payload []byte, wait time.Duration) error {
	head := make([]byte, 2, 14)
	head[0] = 0x80 | op
	switch n := len(payload); {
	case n < 126:
		head[1] = 0x80 | byte(n)
	case n <= 0xffff:
		head[1] = 0x80 | 126
		head = binary.BigEndian.AppendUint16(head, uint16(n))
	default:
		head[1] = 0x80 | 127
		head = binary.BigEndian.AppendUint64(head, uint64(n))
	}
	var mask [4]byte
	_, _ = rand.Read(mask[:])
	head = append(head, mask[:]...)
	frame := make([]byte, len(head)+len(payload))
	copy(frame, head)
	for i, b := range payload {
		frame[len(head)+i] = b ^ mask[i%4]
	}
	w.wmu.Lock()
	defer w.wmu.Unlock()
	_ = w.conn.SetWriteDeadline(time.Now().Add(wait))
	_, err := w.conn.Write(frame)
	return err
}

// close says goodbye, as well as the connection still lets it, and ends
// the connection.
func (w *wsConn) close() {
	w.once.Do(func() {
		// A write held up by a peer that reads nothing gives way soon.
		_ = w.conn.SetWriteDeadline(time.Now().Add(time.Second))
		_ = w.write(opClose, []byte{0x03, 0xe8}, time.Second) // 1000: normal closure
		w.conn.Close()
	})
}
