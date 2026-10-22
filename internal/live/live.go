// Package live tells open management pages that data they show has changed.
// Events carry no data: pages re-fetch their own rows through ordinary,
// authorized requests, so a stream never reveals what changed.
package live

import (
	"io"
	"net/http"
	"sync"
	"time"

	"paymentwebsite/internal/logging"
)

// heartbeat keeps proxies from closing quiet streams; Railway's cutoff is five
// idle minutes.
const heartbeat = 30 * time.Second

// Feed fans committed writes out to every open event stream.
// ponytail: in-process fan-out reaches only this replica's pages; move to
// PostgreSQL LISTEN/NOTIFY before running more than one replica.
type Feed struct {
	mu sync.Mutex
	// streams is nil once Close has ended them.
	streams map[chan struct{}]struct{}
}

func New() *Feed {
	return &Feed{streams: make(map[chan struct{}]struct{})}
}

// Changed wakes every open stream. A stream with an event already pending
// absorbs further wake-ups, so a burst of writes costs each page one refresh.
func (f *Feed) Changed() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for stream := range f.streams {
		select {
		case stream <- struct{}{}:
		default:
		}
	}
}

// Close ends every open stream, and any later one after its first event, so
// graceful shutdown never waits on pages that stay open.
func (f *Feed) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for stream := range f.streams {
		close(stream)
	}
	f.streams = nil
}

// ServeHTTP streams server-sent events: one on connect, so a page catches up
// on anything it missed while disconnected, then one per burst of changes.
func (f *Feed) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	response := http.NewResponseController(w)
	// The server's WriteTimeout bounds whole responses; a stream has no end.
	if err := response.SetWriteDeadline(time.Time{}); err != nil {
		logging.RecordError(r, "stream", err)
		http.Error(w, "Live updates are unavailable.", http.StatusInternalServerError)
		return
	}
	stream := make(chan struct{}, 1)
	stream <- struct{}{}
	f.mu.Lock()
	if f.streams == nil {
		close(stream)
	} else {
		f.streams[stream] = struct{}{}
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.streams, stream)
		f.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()
	for {
		event := ": heartbeat\n\n"
		select {
		case <-r.Context().Done():
			return
		case _, open := <-stream:
			if !open {
				return
			}
			event = "data: change\n\n"
		case <-ticker.C:
		}
		// Write failures mean the page went away, which needs no report.
		if _, err := io.WriteString(w, event); err != nil {
			return
		}
		if err := response.Flush(); err != nil {
			return
		}
	}
}
