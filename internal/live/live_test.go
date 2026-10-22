package live

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Pages stay current only if a stream catches them up on connect, follows
// changes for longer than the server's WriteTimeout, and ends once shutdown
// starts, even for a page connecting late.
func TestStreamCatchesUpFollowsChangesAndEndsOnClose(t *testing.T) {
	feed := New()
	server := httptest.NewUnstartedServer(feed)
	server.Config.WriteTimeout = 100 * time.Millisecond
	server.Start()
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	open := func() *bufio.Reader {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { res.Body.Close() })
		if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatalf("status = %d, Content-Type = %q; want an event stream", res.StatusCode, res.Header.Get("Content-Type"))
		}
		return bufio.NewReader(res.Body)
	}
	requireEvent := func(stream *bufio.Reader, step string) {
		t.Helper()
		var event string
		for range 2 {
			line, err := stream.ReadString('\n')
			if err != nil {
				t.Fatalf("%s: %v", step, err)
			}
			event += line
		}
		if event != "data: change\n\n" {
			t.Fatalf("%s: event = %q, want a change", step, event)
		}
	}
	requireEnd := func(stream *bufio.Reader, step string) {
		t.Helper()
		if _, err := stream.ReadByte(); !errors.Is(err, io.EOF) {
			t.Fatalf("%s: read error = %v, want the stream to end", step, err)
		}
	}

	stream := open()
	requireEvent(stream, "connect")
	time.Sleep(3 * server.Config.WriteTimeout)
	feed.Changed()
	requireEvent(stream, "change after WriteTimeout")
	feed.Close()
	requireEnd(stream, "close")

	late := open()
	requireEvent(late, "connect after close")
	requireEnd(late, "late close")
}
