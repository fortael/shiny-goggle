package profiler

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"sync"
)

// pump buffers whatever the go command writes while the TUI owns the terminal
// and replays it verbatim the moment the terminal is handed back. Nothing is
// dropped: compiler diagnostics and program output always end up on the real
// file descriptors, in order.
type pump struct {
	mu      sync.Mutex
	dst     io.Writer
	pending bytes.Buffer
	live    bool
}

func newPump(dst io.Writer) *pump { return &pump{dst: dst} }

func (p *pump) write(b []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.live {
		_, _ = p.dst.Write(b)

		return
	}

	p.pending.Write(b)
}

// flush replays the buffered bytes without switching to live mode.
func (p *pump) flush() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.writePendingLocked()
}

// release replays the buffer and makes every later write go straight through.
func (p *pump) release() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.writePendingLocked()
	p.live = true
}

func (p *pump) writePendingLocked() {
	if p.pending.Len() == 0 {
		return
	}

	_, _ = p.dst.Write(p.pending.Bytes())
	p.pending.Reset()
}

// copyRaw forwards a byte stream unchanged. It is used for the child's stdout,
// where partial lines matter (prompts, progress written by the built program).
// onFirstByte fires once and tells the driver the build is over and the program
// itself has started talking.
func copyRaw(r io.Reader, p *pump, onFirstByte func()) {
	buf := make([]byte, 32*1024)
	var once sync.Once

	for {
		n, err := r.Read(buf)
		if n > 0 {
			if onFirstByte != nil {
				once.Do(onFirstByte)
			}
			p.write(buf[:n])
		}

		if err != nil {
			return
		}
	}
}

// copyLines forwards a stream line by line, additionally handing every line to
// onLine so the TUI can surface compiler errors and `go: downloading` notices
// while the build is still running.
func copyLines(r io.Reader, p *pump, onLine func(string)) {
	br := bufio.NewReaderSize(r, 64*1024)

	for {
		line, err := br.ReadString('\n')
		if line != "" {
			p.write([]byte(line))
			if onLine != nil {
				onLine(strings.TrimRight(line, "\r\n"))
			}
		}

		if err != nil {
			return
		}
	}
}
