package web

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/wccomps/battleship/internal/auth"
	"github.com/wccomps/battleship/internal/status"
)

// sseWriteTimeout bounds each write to an event stream. battleship serve's HTTP
// server sets no write timeout (one would cut every long-lived stream), so
// this is what keeps a client that stops reading from holding its stream
// (and a hub subscription) for ever.
const sseWriteTimeout = 30 * time.Second

// retryMillis is how long a browser waits before reconnecting an event
// stream that ended.
const retryMillis = 2000

// sseWriter writes server-sent events and flushes each one.
type sseWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

// startSSE sends the headers of an event stream and the reconnect delay.
// It sets a deadline for each write, which also replaces any write timeout
// the HTTP server has (battleship serve sets none) for this stream.
func startSSE(w http.ResponseWriter) (*sseWriter, error) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // nginx: pass events through at once
	sw := &sseWriter{w: w, rc: http.NewResponseController(w)}
	if err := sw.deadline(); err != nil {
		return nil, err
	}
	w.WriteHeader(http.StatusOK)
	if _, err := fmt.Fprintf(w, "retry: %d\n\n", retryMillis); err != nil {
		return nil, err
	}
	return sw, sw.flush()
}

// deadline gives the next write sseWriteTimeout to finish. It uses the real
// clock: it is a network deadline.
func (sw *sseWriter) deadline() error {
	if err := sw.rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}

func (sw *sseWriter) flush() error {
	if err := sw.rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}

// event sends one event. data may span lines; each becomes a data line, and
// the browser joins them with "\n". id, if set, is what the browser sends
// back as Last-Event-ID when it reconnects.
func (sw *sseWriter) event(name, id, data string) error {
	if strings.ContainsAny(name+id, "\r\n") {
		return fmt.Errorf("event name %q or id %q has a line break", name, id)
	}
	var b strings.Builder
	if name != "" {
		b.WriteString("event: " + name + "\n")
	}
	if id != "" {
		b.WriteString("id: " + id + "\n")
	}
	// The browser ends a line at CRLF, LF or a lone CR, and html/template
	// leaves CRs in text (a job's error, a log line): send each as LF, or
	// the browser would drop the rest of its line.
	data = strings.ReplaceAll(strings.ReplaceAll(data, "\r\n", "\n"), "\r", "\n")
	for line := range strings.SplitSeq(data, "\n") {
		b.WriteString("data: " + line + "\n")
	}
	b.WriteString("\n")
	if err := sw.deadline(); err != nil {
		return err
	}
	if _, err := io.WriteString(sw.w, b.String()); err != nil {
		return err
	}
	return sw.flush()
}

// piece wraps one piece of markup an event carries in its own <template>,
// whose content the browser parses in the context of its first tag. Pieces
// parsed together break each other: a <td> after a <div> loses its tag.
func piece(html string) string {
	return "<template>" + html + "</template>"
}

// pieceSet is a page's swappable pieces, so a stream can send just the
// ones that changed: each element's markup by its id, in page order.
type pieceSet struct {
	ids  []string
	html map[string]string
}

// add appends the piece of element id.
func (p *pieceSet) add(id, html string) {
	if p.html == nil {
		p.html = map[string]string{}
	}
	p.ids = append(p.ids, id)
	p.html[id] = html
}

// changedSince returns the markup of the pieces that differ from before,
// or that before lacks, in page order, each in its own <template>.
func (p pieceSet) changedSince(before pieceSet) string {
	var b strings.Builder
	for _, id := range p.ids {
		if h, ok := before.html[id]; !ok || h != p.html[id] {
			b.WriteString(piece(p.html[id]))
			b.WriteString("\n")
		}
	}
	return b.String()
}

// comment sends a comment line, which browsers ignore; it keeps an idle
// stream open through proxies.
func (sw *sseWriter) comment(text string) error {
	if err := sw.deadline(); err != nil {
		return err
	}
	if _, err := io.WriteString(sw.w, ": "+strings.ReplaceAll(text, "\n", " ")+"\n\n"); err != nil {
		return err
	}
	return sw.flush()
}

// stream serves hub topics as an event stream; they share one subscription,
// so a message on any of them makes a send. send writes the current state:
// once at the start with resync true, then after each message, with
// resync true if it was a Resync. A send comes at least gap after the last
// one; messages that arrive meanwhile join it (the hub merges them).
// send's seq is the hub sequence number its state must be read at or after
// (see seqFlight).
//
// The stream subscribes before the first send, so nothing that happens
// between the two is missed. It sends a heartbeat comment every 15s, and
// ends when:
//
//   - the client goes away, or send fails;
//   - CloseStreams is called (server shutdown);
//   - the hub stops, closing the subscription, so the browser reconnects
//     and starts from the current state (the hub never drops a stream
//     for falling behind: it coalesces what the stream hasn't read);
//   - the session's next group check falls due (web.session_refresh after
//     the last one, at most session_refresh after the stream opened, plus
//     up to a tenth more at random; see streamLifetime), so the browser
//     reconnects through the login check, which ends the updates of
//     someone who lost access;
//   - a write takes longer than sseWriteTimeout.
//
// After CloseStreams it refuses new streams with 503. A HEAD request gets
// the headers only.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, topics []string, gap time.Duration, send func(sw *sseWriter, resync bool, seq uint64) error) {
	select {
	case <-s.closed:
		w.Header().Set("Retry-After", "5")
		http.Error(w, "the server is shutting down", http.StatusServiceUnavailable)
		return
	default:
	}
	if r.Method == http.MethodHead {
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		return
	}
	msgs, cancel := s.hub.SubscribeTopics(topics...)
	defer cancel()
	seq := s.hub.Seq() // after subscribing: later changes reach msgs
	sw, err := startSSE(w)
	if err != nil {
		return
	}
	if err := send(sw, true, seq); err != nil {
		return
	}
	end := s.clock.After(s.streamLifetime(auth.SessionRefreshedAt(r.Context())))
	beat := s.clock.After(heartbeatEvery)
	ctx := r.Context()
	// After a send, in is nil until open fires gap later: what comes
	// meanwhile waits in the hub's one slot for this stream, merged into
	// one message, which makes the next send.
	var in <-chan status.Msg
	open := s.clock.After(gap)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.closed:
			return
		case <-end:
			return
		case <-beat:
			select {
			case <-end: // due at the same moment: end rather than beat
				return
			default:
			}
			beat = s.clock.After(heartbeatEvery)
			if err := sw.comment("heartbeat"); err != nil {
				return
			}
		case <-open:
			in, open = msgs, nil
		case m, ok := <-in:
			if !ok {
				return
			}
			if err := send(sw, m.Resync, m.Seq); err != nil {
				return
			}
			in, open = nil, s.clock.After(gap)
		}
	}
}
