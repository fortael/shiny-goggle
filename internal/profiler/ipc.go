package profiler

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// sockEnv carries the driver's unix socket path down to the toolexec shims.
// Using an explicit environment variable (instead of guessing a parent pid)
// makes concurrent builds in the same checkout impossible to confuse.
const sockEnv = "SHINY_GOGGLES_SOCK"

const (
	kindStart = "s"
	kindEnd   = "e"
)

// wireEvent is what a shim sends to the driver. Field names are single letters
// on purpose: a cold build of a mid-sized service emits a few thousand of them.
type wireEvent struct {
	Kind string  `json:"k"`
	Tool string  `json:"t,omitempty"`
	Pkg  string  `json:"p,omitempty"`
	Std  bool    `json:"s,omitempty"`
	Dur  float64 `json:"d,omitempty"`
	Fail bool    `json:"f,omitempty"`
	// Compiler internals, from `compile -bench`.
	Fe    float64 `json:"fe,omitempty"` // frontend seconds
	Be    float64 `json:"be,omitempty"` // backend seconds
	Lines int     `json:"l,omitempty"`
	Funcs int     `json:"n,omitempty"`
}

// startEvent and endEvent are the driver-side representation of wireEvent.
// The id is assigned per connection, so a shim never has to invent one.
type startEvent struct {
	id   uint64
	tool string
	pkg  string
	std  bool
}

type endEvent struct {
	id       uint64
	dur      time.Duration
	fail     bool
	aborted  bool
	frontend time.Duration
	backend  time.Duration
	lines    int
	funcs    int
}

// eventServer accepts one connection per tool invocation. The connection stays
// open for the lifetime of that invocation, so a shim that is killed mid-way is
// reported as aborted instead of hanging around in the "running" list forever.
type eventServer struct {
	ln   net.Listener
	path string
}

func listenEvents(handle func(any)) (*eventServer, error) {
	path := filepath.Join(os.TempDir(), fmt.Sprintf("shiny-goggles-%d.sock", os.Getpid()))
	_ = os.Remove(path)

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", path, err)
	}

	srv := &eventServer{ln: ln, path: path}

	go func() {
		var seq atomic.Uint64

		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			go serveConn(conn, seq.Add(1), handle)
		}
	}()

	return srv, nil
}

func (s *eventServer) close() {
	if s == nil {
		return
	}

	_ = s.ln.Close()
	_ = os.Remove(s.path)
}

func serveConn(conn net.Conn, id uint64, handle func(any)) {
	defer func() { _ = conn.Close() }()

	dec := json.NewDecoder(conn)
	started, ended := false, false

	for {
		var ev wireEvent
		if err := dec.Decode(&ev); err != nil {
			break
		}

		switch ev.Kind {
		case kindStart:
			started = true
			handle(startEvent{id: id, tool: ev.Tool, pkg: ev.Pkg, std: ev.Std})
		case kindEnd:
			ended = true
			handle(endEvent{
				id:       id,
				dur:      time.Duration(ev.Dur * float64(time.Second)),
				fail:     ev.Fail,
				frontend: time.Duration(ev.Fe * float64(time.Second)),
				backend:  time.Duration(ev.Be * float64(time.Second)),
				lines:    ev.Lines,
				funcs:    ev.Funcs,
			})
		}
	}

	if started && !ended {
		handle(endEvent{id: id, aborted: true})
	}
}

// eventClient is the shim side of the protocol. Every method tolerates a nil
// receiver: when there is no driver listening the shim degrades to a plain
// exec wrapper instead of failing the build.
type eventClient struct {
	conn net.Conn
	enc  *json.Encoder
}

func dialEvents() *eventClient {
	path := os.Getenv(sockEnv)
	if path == "" {
		return nil
	}

	conn, err := net.DialTimeout("unix", path, 500*time.Millisecond)
	if err != nil {
		return nil
	}

	return &eventClient{conn: conn, enc: json.NewEncoder(conn)}
}

func (c *eventClient) send(ev wireEvent) {
	if c == nil {
		return
	}

	_ = c.enc.Encode(ev)
}

func (c *eventClient) close() {
	if c == nil {
		return
	}

	_ = c.conn.Close()
}
