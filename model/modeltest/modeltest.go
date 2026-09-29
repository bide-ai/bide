// Package modeltest checks that an HTTP-backed agent.Model releases its response when the
// consumer stops early. An adapter that reads a streamed response in its own goroutine and
// sends each event on an unbuffered channel blocks forever once nobody is receiving, holding
// the response body and its connection. Run holds an adapter to the contract: breaking out of
// Stream.Events, or cancelling the context passed to Model.Stream, closes the response body.
package modeltest

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// Run runs the release checks. newModel returns the adapter under test pointed at baseURL and
// using client for its requests. event returns the i-th server-sent event of an endless text
// response, in the provider's wire format and including its trailing blank line; the test server
// sends one every few milliseconds until the client goes away, as a long model response does.
func Run(t *testing.T, newModel func(baseURL string, client *http.Client) agent.Model, event func(i int) string) {
	t.Run("BreakReleases", func(t *testing.T) {
		m, closed := serve(t, newModel, event)
		s, err := m.Stream(context.Background(), request())
		if err != nil {
			t.Fatal(err)
		}
		for _, err := range s.Events() {
			if err != nil {
				t.Fatal(err)
			}
			break // the consumer has what it needs; its context stays live
		}
		wait(t, closed, "the consumer broke out of Events")
	})
	t.Run("CancelReleases", func(t *testing.T) {
		m, closed := serve(t, newModel, event)
		ctx, cancel := context.WithCancel(context.Background())
		if _, err := m.Stream(ctx, request()); err != nil {
			t.Fatal(err)
		}
		cancel() // the consumer abandons the stream without reading it
		wait(t, closed, "the stream's context was cancelled")
	})
}

func request() agent.Request {
	return agent.Request{Messages: []agent.Message{agent.UserText("hi")}}
}

// serve starts an endless SSE server and returns the adapter under test, plus a channel closed
// once the adapter closes the response body.
func serve(t *testing.T, newModel func(string, *http.Client) agent.Model, event func(int) string) (agent.Model, <-chan struct{}) {
	t.Helper()
	// stop ends the handler at cleanup, so a leaked reader holding the connection fails the
	// test on its assertion instead of hanging the server's shutdown.
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		for i := 0; ; i++ {
			if _, err := io.WriteString(w, event(i)); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}))
	t.Cleanup(func() { close(stop); srv.CloseClientConnections(); srv.Close() })
	closed := make(chan struct{})
	client := &http.Client{Transport: bodyCloseHook{next: http.DefaultTransport, closed: closed}}
	return newModel(srv.URL, client), closed
}

func wait(t *testing.T, closed <-chan struct{}, after string) {
	t.Helper()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatalf("the response body is still open 2s after %s: the adapter's reader is not released", after)
	}
}

// bodyCloseHook signals when the adapter closes the response body.
type bodyCloseHook struct {
	next   http.RoundTripper
	closed chan struct{}
}

func (h bodyCloseHook) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := h.next.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("modeltest: server answered %s", resp.Status)
	}
	resp.Body = &hookedBody{ReadCloser: resp.Body, closed: h.closed}
	return resp, nil
}

type hookedBody struct {
	io.ReadCloser
	once   sync.Once
	closed chan struct{}
}

func (b *hookedBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return b.ReadCloser.Close()
}
