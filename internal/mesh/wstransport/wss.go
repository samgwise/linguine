package wstransport

import (
	"go.nanomsg.org/mangos/v3"
	"go.nanomsg.org/mangos/v3/transport"
)

// WSS is the WebSocket-over-TLS transport (scheme "wss"). It is a thin alias
// over WS — the ws transport already branches on the wss:// scheme for both
// dial and listen — existing only so mangos' transport registry has a value
// to return for GetTransport("wss").
type wssTran int

// WSS is the secure WebSocket transport.
const WSS = wssTran(0)

func init() {
	transport.RegisterTransport(WSS)
}

func (wssTran) Scheme() string { return "wss" }

func (w wssTran) NewDialer(addr string, sock mangos.Socket) (transport.Dialer, error) {
	return WS.NewDialer(addr, sock)
}

func (w wssTran) NewListener(addr string, sock mangos.Socket) (transport.Listener, error) {
	return WS.NewListener(addr, sock)
}
