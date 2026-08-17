// Package wstransport is a vendored copy of mangos' WebSocket transport
// (go.nanomsg.org/mangos/v3/transport/ws and .../wss) with one change: the
// dialer honours a forward proxy. The upstream dialer builds a bare
// gorilla *websocket.Dialer with no Proxy field, so HTTP_PROXY/HTTPS_PROXY
// are ignored and workers behind a corporate HTTP CONNECT proxy cannot
// egress. Here the dialer defaults to http.ProxyFromEnvironment and can be
// overridden per-dial with OptionWebSocketProxyURL.
//
// The rest of the file is reproduced faithfully so the xrep/xreq pipe
// routing, the mangos backtrace protocol, and the subprotocol handshake all
// behave as upstream. Vendoring just this transport (rather than forking the
// whole module) keeps the change minimal and the upgrade path simple.
package wstransport

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/gorilla/websocket"

	"go.nanomsg.org/mangos/v3"
	"go.nanomsg.org/mangos/v3/transport"
)

// Some special options
const (
	// OptionWebSocketMux is a retrieve-only property used to obtain
	// the *http.ServeMux instance associated with the server.  This
	// can be used to subsequently register additional handlers for
	// different URIs.  This option is only valid on a Listener.
	// Generally you use this option when you want to use the standard
	// mangos Listen() method to start up the server.
	OptionWebSocketMux = "WEBSOCKET-MUX"

	// OptionWebSocketHandler is used to obtain the underlying
	// http.Handler (websocket.Server) object, so you can use this
	// on your own http.Server instances.  It is a gross error to use
	// the value returned by this method on an http server if the
	// server is also started with mangos Listen().  This means that
	// you will use at most either this option, or OptionWebSocketMux, but
	// never both.  This option is only valid on a listener.
	OptionWebSocketHandler = "WEBSOCKET-HANDLER"

	// OptionWebSocketCheckOrigin controls the check of the origin of the
	// underlying Listener (websocket.Upgrader).
	OptionWebSocketCheckOrigin = "WEBSOCKET-CHECKORIGIN"

	// OptionWebSocketProxyURL is a *url.URL the dialer tunnels through
	// using HTTP CONNECT. When set it takes precedence over the
	// HTTP_PROXY/HTTPS_PROXY/NO_PROXY environment (which is the default).
	// This lets a worker pin a specific forward proxy regardless of env.
	OptionWebSocketProxyURL = "WEBSOCKET-PROXY-URL"

	// WS is the plain WebSocket transport (scheme "ws").
	WS = wsTran(0)
)

type options map[string]interface{}

func init() {
	transport.RegisterTransport(WS)
}

// GetOption retrieves an option value.
func (o options) get(name string) (interface{}, error) {
	if name == mangos.OptionNoDelay {
		return true, nil
	}
	v, ok := o[name]
	if !ok {
		return nil, mangos.ErrBadOption
	}
	return v, nil
}

// SetOption sets an option.
func (o options) set(name string, val interface{}) error {
	switch name {
	case mangos.OptionNoDelay:
		if _, ok := val.(bool); ok {
			return nil
		}
		return mangos.ErrBadValue
	case OptionWebSocketCheckOrigin:
		if v, ok := val.(bool); ok {
			o[name] = v
			return nil
		}
		return mangos.ErrBadValue
	case mangos.OptionTLSConfig:
		if v, ok := val.(*tls.Config); ok {
			o[name] = v
			return nil
		}
		return mangos.ErrBadValue
	case OptionWebSocketProxyURL:
		if v, ok := val.(*url.URL); ok {
			o[name] = v
			return nil
		}
		return mangos.ErrBadValue
	case mangos.OptionMaxRecvSize:
		if v, ok := val.(int); ok {
			o[name] = v
			return nil
		}
		return mangos.ErrBadValue
	}
	return mangos.ErrBadOption
}

// wsPipe implements the Pipe interface on a websocket
type wsPipe struct {
	ws      *websocket.Conn
	proto   transport.ProtocolInfo
	addr    string
	open    bool
	wg      sync.WaitGroup
	options map[string]interface{}
	iswss   bool
	dtype   int
	sync.Mutex
}

type wsTran int

func (w *wsPipe) Recv() (*mangos.Message, error) {

	// We ignore the message type for receive.
	_, body, err := w.ws.ReadMessage()
	if err != nil {
		return nil, err
	}
	msg := mangos.NewMessage(0)
	msg.Body = body
	return msg, nil
}

func (w *wsPipe) Send(m *mangos.Message) error {

	var buf []byte

	if len(m.Header) > 0 {
		buf = make([]byte, 0, len(m.Header)+len(m.Body))
		buf = append(buf, m.Header...)
		buf = append(buf, m.Body...)
	} else {
		buf = m.Body
	}
	if err := w.ws.WriteMessage(w.dtype, buf); err != nil {
		return err
	}
	m.Free()
	return nil
}

func (w *wsPipe) Close() error {
	w.Lock()
	defer w.Unlock()
	if w.open {
		w.open = false
		_ = w.ws.Close()
		w.wg.Done()
	}
	return nil
}

func (w *wsPipe) GetOption(name string) (interface{}, error) {
	if v, ok := w.options[name]; ok {
		return v, nil
	}
	return nil, mangos.ErrBadOption
}

type dialer struct {
	addr  string // url
	proto mangos.ProtocolInfo
	opts  options
	iswss bool
}

func (d *dialer) Dial() (transport.Pipe, error) {
	var w *wsPipe

	wd := &websocket.Dialer{}

	wd.Subprotocols = []string{d.proto.PeerName + ".sp.nanomsg.org"}
	if v, ok := d.opts[mangos.OptionTLSConfig]; ok {
		wd.TLSClientConfig = v.(*tls.Config)
	}

	// Forward-proxy egress. An explicit OptionWebSocketProxyURL takes
	// precedence; otherwise fall back to the standard proxy environment
	// variables. The upstream mangos dialer leaves Proxy nil (gorilla then
	// uses no proxy), which is why env-var proxying did not work — this is
	// the behaviour we vendored this transport to fix.
	wd.Proxy = http.ProxyFromEnvironment
	if v, ok := d.opts[OptionWebSocketProxyURL]; ok {
		if pu, ok := v.(*url.URL); ok && pu != nil {
			wd.Proxy = http.ProxyURL(pu)
		}
	}

	w = &wsPipe{
		addr:    d.addr,
		proto:   d.proto,
		open:    true,
		dtype:   websocket.BinaryMessage,
		options: make(map[string]interface{}),
	}

	maxrx := 0
	v, err := d.opts.get(mangos.OptionMaxRecvSize)
	if err == nil {
		maxrx, _ = v.(int)
	}
	if w.ws, _, err = wd.Dial(d.addr, nil); err != nil {
		if err == websocket.ErrBadHandshake {
			return nil, mangos.ErrBadProto
		}
		return nil, err
	}
	w.ws.SetReadLimit(int64(maxrx))
	w.options[mangos.OptionLocalAddr] = w.ws.LocalAddr()
	w.options[mangos.OptionRemoteAddr] = w.ws.RemoteAddr()
	if tlsConn, ok := w.ws.UnderlyingConn().(*tls.Conn); ok {
		w.options[mangos.OptionTLSConnState] = tlsConn.ConnectionState()
	}

	w.wg.Add(1)
	return w, nil
}

func (d *dialer) SetOption(n string, v interface{}) error {
	return d.opts.set(n, v)
}

func (d *dialer) GetOption(n string) (interface{}, error) {
	return d.opts.get(n)
}

type listener struct {
	pending  []*wsPipe
	lock     sync.Mutex
	cv       sync.Cond
	running  bool
	noserve  bool
	addr     string
	bound    *net.TCPAddr
	anon     bool // anonymous port selected
	closed   bool
	ug       websocket.Upgrader
	htsvr    *http.Server
	mux      *http.ServeMux
	url      *url.URL
	listener net.Listener
	proto    transport.ProtocolInfo
	opts     options
	iswss    bool
}

func (l *listener) SetOption(n string, v interface{}) error {
	switch n {
	case OptionWebSocketCheckOrigin:
		if v, ok := v.(bool); ok {
			if !v {
				l.ug.CheckOrigin = func(r *http.Request) bool { return true }
			} else {
				l.ug.CheckOrigin = nil
			}
		}
	}
	return l.opts.set(n, v)
}

func (l *listener) GetOption(n string) (interface{}, error) {
	switch n {
	case OptionWebSocketMux:
		return l.mux, nil
	case OptionWebSocketHandler:
		// Caller intends to use use in his own server, so mark
		// us running.  If he didn't mean this, the side effect is
		// that Accept() will appear to hang, even though Listen()
		// is not called yet.
		l.running = true
		l.noserve = true
		return l, nil
	case OptionWebSocketCheckOrigin:
		if v, err := l.opts.get(n); err == nil {
			if v, ok := v.(bool); ok {
				return v, nil
			}
		}
		return true, nil

	}
	return l.opts.get(n)
}

func (l *listener) Listen() error {
	var taddr *net.TCPAddr
	var err error
	var tcfg *tls.Config

	if l.closed {
		return mangos.ErrClosed
	}
	if l.noserve {
		// The HTTP framework is going to call us, so we use that rather than
		// listening on our own.  We just fake this out.
		return nil
	}
	if l.iswss {
		v, ok := l.opts[mangos.OptionTLSConfig]
		if !ok || v == nil {
			return mangos.ErrTLSNoConfig
		}
		tcfg = v.(*tls.Config)
		if len(tcfg.Certificates) == 0 {
			return mangos.ErrTLSNoCert
		}
	}

	// We listen separately, that way we can catch and deal with the
	// case of a port already in use.  This also lets us configure
	// properties of the underlying TCP connection.

	if taddr, err = transport.ResolveTCPAddr(l.url.Host); err != nil {
		return err
	}

	if taddr.Port == 0 {
		l.anon = true
	}
	if tlist, err := net.ListenTCP("tcp", taddr); err != nil {
		return err
	} else if l.iswss {
		l.listener = tls.NewListener(tlist, tcfg)
	} else {
		l.listener = tlist
	}
	l.pending = nil
	l.running = true
	l.bound = l.listener.Addr().(*net.TCPAddr)

	l.htsvr = &http.Server{Addr: l.url.Host, Handler: l.mux}

	go func() {
		_ = l.htsvr.Serve(l.listener)
	}()

	return nil
}

func (l *listener) Accept() (transport.Pipe, error) {
	var w *wsPipe

	l.lock.Lock()
	defer l.lock.Unlock()

	for {
		if !l.running {
			return nil, mangos.ErrClosed
		}
		if len(l.pending) == 0 {
			l.cv.Wait()
			continue
		}
		w = l.pending[len(l.pending)-1]
		l.pending = l.pending[:len(l.pending)-1]
		break
	}

	return w, nil
}

func (l *listener) handler(ws *websocket.Conn, req *http.Request) {
	l.lock.Lock()

	w := &wsPipe{
		ws:      ws,
		addr:    l.addr,
		proto:   l.proto,
		open:    true,
		dtype:   websocket.BinaryMessage,
		iswss:   l.iswss,
		options: make(map[string]interface{}),
	}
	maxRx := 0
	v, err := l.opts.get(mangos.OptionMaxRecvSize)
	if err == nil {
		maxRx, _ = v.(int)
	}

	w.ws.SetReadLimit(int64(maxRx))
	w.options[mangos.OptionLocalAddr] = ws.LocalAddr()
	w.options[mangos.OptionRemoteAddr] = ws.RemoteAddr()

	if req.TLS != nil {
		w.options[mangos.OptionTLSConnState] = *req.TLS
	}

	w.wg.Add(1)
	l.pending = append(l.pending, w)
	l.cv.Broadcast()
	l.lock.Unlock()

	// We must not return before the socket is closed, because
	// our caller will close the websocket on our return.
	w.wg.Wait()
}

func (l *listener) Close() error {
	l.lock.Lock()
	defer l.lock.Unlock()
	if l.closed {
		return mangos.ErrClosed
	}
	if l.listener != nil {
		_ = l.listener.Close()
	}
	l.closed = true
	l.running = false
	l.cv.Broadcast()
	for _, ws := range l.pending {
		_ = ws.Close()
	}
	l.pending = nil
	return nil
}

func (l *listener) ServeHTTP(w http.ResponseWriter, r *http.Request) {

	matched := false
	for _, subProto := range websocket.Subprotocols(r) {
		if subProto == l.proto.SelfName+".sp.nanomsg.org" {
			matched = true
		}
	}
	if !matched {
		http.Error(w, "SP protocol mis-match", http.StatusBadRequest)
		return
	}
	l.lock.Lock()
	if !l.running {
		l.lock.Unlock()
		http.Error(w, "No handler at that address", http.StatusNotFound)
		return
	}
	l.lock.Unlock()
	ws, err := l.ug.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	l.handler(ws, r)
}

func (l *listener) Address() string {
	if l.anon {
		u := l.url
		u.Host = fmt.Sprintf("%s:%d", u.Hostname(), l.bound.Port)
		return u.String()
	}
	return l.url.String()
}

func (wsTran) Scheme() string {
	return "ws"
}

func (wsTran) NewDialer(addr string, sock mangos.Socket) (transport.Dialer, error) {
	d := &dialer{
		addr:  addr,
		proto: sock.Info(),
		iswss: false,
		opts:  make(map[string]interface{}),
	}

	if strings.HasPrefix(addr, "wss://") {
		d.iswss = true
	} else if !strings.HasPrefix(addr, "ws://") {
		return nil, mangos.ErrBadTran
	}

	d.opts[mangos.OptionNoDelay] = true
	d.opts[mangos.OptionMaxRecvSize] = 0

	return d, nil
}

func (t wsTran) NewListener(addr string, sock mangos.Socket) (transport.Listener, error) {
	l, e := t.listener(addr, sock)
	if e != nil {
		return nil, e
	}
	l.mux.Handle(l.url.Path, l)
	return l, nil
}

func (t wsTran) listener(addr string, sock mangos.Socket) (*listener, error) {
	var err error
	l := &listener{
		addr:  addr,
		proto: sock.Info(),
		opts:  make(map[string]interface{}),
	}
	l.opts[mangos.OptionMaxRecvSize] = 0
	l.cv.L = &l.lock
	l.ug.Subprotocols = []string{l.proto.SelfName + ".sp.nanomsg.org"}

	if strings.HasPrefix(addr, "wss://") {
		l.iswss = true
	} else if !strings.HasPrefix(addr, "ws://") {
		return nil, mangos.ErrBadTran
	}

	if l.url, err = url.ParseRequestURI(addr); err != nil {
		return nil, err
	}
	if len(l.url.Path) == 0 {
		l.url.Path = "/"
	}
	l.mux = http.NewServeMux()

	l.htsvr = &http.Server{Addr: l.url.Host, Handler: l.mux}

	return l, nil
}
