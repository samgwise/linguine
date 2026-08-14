package router

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"go.nanomsg.org/mangos/v3"

	"github.com/samgw/linguine/internal/auth"
	"github.com/samgw/linguine/internal/config"
	"github.com/samgw/linguine/internal/mesh"
	"github.com/samgw/linguine/internal/protocol"
)

const (
	sessionBufferSize = 256
	nodeStaleAfter    = 15 * time.Second
)

// Deps holds the router's external dependencies.
type Deps struct {
	NNG         *mesh.Router
	Signer      *auth.Signer
	Keys        *auth.APIKeyRepo
	Enrollments *auth.EnrollmentRepo
	HTTPListen  string
	TLS         config.TLSFiles
	StaleAfter  time.Duration // 0 uses the default (nodeStaleAfter)
}

// Server is the central control plane: a Fiber OpenAI-compatible ingress
// that authenticates clients, selects a worker, and proxies streamed
// completions over the NNG mesh.
type Server struct {
	nng         *mesh.Router
	signer      *auth.Signer
	keys        *auth.APIKeyRepo
	enrollments *auth.EnrollmentRepo
	nodes       *nodeRegistry
	sessions    sync.Map // reqID -> chan *protocol.Frame
	app         *fiber.App
	httpListen  string
	tls         config.TLSFiles
	ln          net.Listener
}

// New constructs a router from its dependencies.
func New(deps Deps) *Server {
	app := fiber.New()
	stale := deps.StaleAfter
	if stale == 0 {
		stale = nodeStaleAfter
	}
	s := &Server{
		nng:         deps.NNG,
		signer:      deps.Signer,
		keys:        deps.Keys,
		enrollments: deps.Enrollments,
		nodes:       newNodeRegistry(stale),
		app:         app,
		httpListen:  deps.HTTPListen,
		tls:         deps.TLS,
	}
	s.registerRoutes()
	return s
}

// App returns the underlying Fiber app (for in-process testing).
func (s *Server) App() *fiber.App { return s.app }

func (s *Server) registerRoutes() {
	s.app.Post("/v1/chat/completions", s.handleChatCompletions)
}

// Start runs the NNG receive loop and HTTP server on a new listener, returning
// the listener so the caller can dial it. It is non-blocking.
func (s *Server) Start(ctx context.Context) (net.Listener, error) {
	go s.recvLoop(ctx)
	ln, err := net.Listen("tcp", s.httpListen)
	if err != nil {
		return nil, fmt.Errorf("router: listen: %w", err)
	}
	s.ln = ln
	go func() {
		var serveErr error
		if s.tls.Enabled {
			cfg, cerr := tlsConfig(s.tls)
			if cerr != nil {
				log.Printf("[router] tls config: %v", cerr)
				return
			}
			serveErr = s.app.Listener(tls.NewListener(ln, cfg))
		} else {
			serveErr = s.app.Listener(ln)
		}
		if serveErr != nil {
			log.Printf("[router] http serve: %v", serveErr)
		}
	}()
	return ln, nil
}

// Shutdown stops the HTTP server and closes the mesh socket.
func (s *Server) Shutdown() error {
	if err := s.app.Shutdown(); err != nil {
		return fmt.Errorf("router: http shutdown: %w", err)
	}
	return s.nng.Close()
}

func (s *Server) recvLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		msg, err := s.nng.Recv()
		if err != nil {
			if errors.Is(err, mangos.ErrClosed) {
				return
			}
			if errors.Is(err, mangos.ErrRecvTimeout) {
				continue
			}
			log.Printf("[router] recv: %v", err)
			continue
		}
		s.handleMeshMessage(msg)
		msg.Free()
	}
}

func (s *Server) handleMeshMessage(msg *mangos.Message) {
	env, err := protocol.DecodeEnvelope(msg.Body)
	if err != nil {
		return
	}
	pipe, err := mesh.PipeFromHeader(msg.Header)
	if err != nil {
		return
	}
	if env.ReqID == protocol.HeartbeatReqID {
		s.handleHeartbeat(pipe, env)
		return
	}
	f, err := protocol.DecodeFrame(env.Payload)
	if err != nil {
		return
	}
	if v, ok := s.sessions.Load(env.ReqID); ok {
		ch := v.(chan *protocol.Frame)
		select {
		case ch <- f:
		default:
			// Backpressure: a slow client's session buffer is full. Dropping
			// a frame corrupts that one stream; proper flow control is a
			// later-phase concern. Avoid blocking the shared recv loop.
			log.Printf("[router] drop frame for %s (session backpressure)", env.ReqID)
		}
	}
}

func (s *Server) handleHeartbeat(pipe mesh.PipeID, env *protocol.Envelope) {
	var hb protocol.Heartbeat
	if err := json.Unmarshal(env.Payload, &hb); err != nil {
		return
	}
	enr, err := s.signer.Parse(hb.EnrollmentToken)
	if err != nil {
		log.Printf("[router] heartbeat auth failed from pipe %d: %v", pipe, err)
		return
	}
	active, err := s.enrollments.IsActive(context.Background(), enr.ID)
	if err != nil {
		log.Printf("[router] enrollment check: %v", err)
		return
	}
	if !active {
		log.Printf("[router] enrollment %s not active", enr.ID)
		return
	}
	s.nodes.upsert(&nodeEntry{
		id:          enr.NodeName,
		pipe:        pipe,
		activeModel: hb.ActiveModel,
		lastSeen:    time.Now(),
	})
}

func (s *Server) handleChatCompletions(c fiber.Ctx) error {
	if _, err := s.authenticate(c); err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": fiber.Map{"message": err.Error()}})
	}
	body := c.Body()
	node, ok := s.nodes.selectAny()
	if !ok {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": fiber.Map{"message": "no worker available"}})
	}
	reqID := uuid.NewString()
	ch := make(chan *protocol.Frame, sessionBufferSize)
	s.sessions.Store(reqID, ch)
	env := &protocol.Envelope{ReqID: reqID, Payload: body}
	if err := s.nng.SendTo(node.pipe, env.Encode()); err != nil {
		s.sessions.Delete(reqID)
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": fiber.Map{"message": "dispatch failed"}})
	}
	if isStream(body) {
		return s.streamResponse(c, reqID, ch)
	}
	return s.bufferedResponse(c, reqID, ch)
}

func (s *Server) authenticate(c fiber.Ctx) (*auth.APIKey, error) {
	token := extractBearer(c.Get(fiber.HeaderAuthorization))
	if token == "" {
		return nil, errors.New("missing api key")
	}
	return s.keys.Verify(c.Context(), token)
}

func (s *Server) streamResponse(c fiber.Ctx, reqID string, ch chan *protocol.Frame) error {
	c.Set(fiber.HeaderContentType, fiber.MIMETextEventStream)
	c.Set(fiber.HeaderCacheControl, "no-cache")
	c.Set(fiber.HeaderConnection, "keep-alive")
	c.Abandon() // keep the ctx alive for the async stream writer
	return c.SendStreamWriter(func(w *bufio.Writer) {
		defer s.sessions.Delete(reqID)
		for f := range ch {
			switch f.Type {
			case protocol.FrameTypeChunk:
				_, _ = w.Write(f.Payload)
				if err := w.Flush(); err != nil {
					return // client disconnected
				}
			case protocol.FrameTypeEOF:
				return
			case protocol.FrameTypeError:
				fmt.Fprintf(w, "data: {\"error\":{\"message\":%q}}\n\n", string(f.Payload))
				_ = w.Flush()
				return
			}
		}
	})
}

func (s *Server) bufferedResponse(c fiber.Ctx, reqID string, ch chan *protocol.Frame) error {
	defer s.sessions.Delete(reqID)
	var buf bytes.Buffer
	for f := range ch {
		switch f.Type {
		case protocol.FrameTypeChunk:
			buf.Write(f.Payload)
		case protocol.FrameTypeEOF:
			c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
			return c.Status(fiber.StatusOK).Send(buf.Bytes())
		case protocol.FrameTypeError:
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": fiber.Map{"message": string(f.Payload)}})
		}
	}
	return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": fiber.Map{"message": "upstream stream closed"}})
}

func extractBearer(authz string) string {
	const prefix = "Bearer "
	if len(authz) > len(prefix) && strings.EqualFold(authz[:len(prefix)], prefix) {
		return authz[len(prefix):]
	}
	return ""
}

func isStream(body []byte) bool {
	var v struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return false
	}
	return v.Stream
}

func tlsConfig(t config.TLSFiles) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("router: load server keypair: %w", err)
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}}
	if t.CAFile != "" {
		caPEM, err := os.ReadFile(t.CAFile)
		if err != nil {
			return nil, fmt.Errorf("router: load ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, errors.New("router: no certs in ca file")
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}
