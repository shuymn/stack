package jev

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// sentinel stands for input a response body might quote; it must never reach an outcome.
const sentinel = "SENTINEL-quoted-input-7f3a"

var noulQ = []Question{{Name: "q", Type: TypeNoul}}

func noulBody(model string, p string) string {
	return fmt.Sprintf(`{"model":%q,"answers":{"q":{"type":"noul","noul":%s}},`+
		`"usage":{"input_tokens":10,"output_tokens":1},"echo":%q}`, model, p, sentinel)
}

func testBatch(qs []Question) Batch {
	body := []byte(`{"model":"jev-1.14.0","state":"s","questions":{}}`)
	return Batch{Body: body, Hash: Hash(body), Questions: qs}
}

func TestClientErrors(t *testing.T) {
	choiceQ := []Question{{Name: "q", Type: TypeChoice, Labels: []string{"a", "b"}}}
	cases := []struct {
		name    string
		status  int
		body    string
		qs      []Question
		class   Class
		cause   string
		delay   time.Duration
		bodyGap time.Duration
	}{
		{"ok noul", 200, noulBody(Pin, "0.83"), noulQ, OK, "", 0, 0},
		{"ok choice", 200, `{"model":"jev-1.14.0","answers":{"q":{"type":"choice","choice":"a",` +
			`"confidence":0.7,"probabilities":{"a":0.7,"b":0.3}}}}`, choiceQ, OK, "", 0, 0},
		{"429", 429, sentinel, noulQ, Transient, "HTTP 429", 0, 0},
		{"529", 529, sentinel, noulQ, Transient, "HTTP 529", 0, 0},
		{"503", 503, sentinel, noulQ, Transient, "HTTP 503", 0, 0},
		{
			"400 max tokens", 400, `{"detail":"max_tokens_exceeded: ` + sentinel + `"}`, noulQ,
			TooLarge, CauseMaxTokens, 0, 0,
		},
		{"413", 413, sentinel, noulQ, TooLarge, "HTTP 413", 0, 0},
		{"400 other", 400, `{"detail":"` + sentinel + `"}`, noulQ, Persistent, "HTTP 400", 0, 0},
		{"401", 401, sentinel, noulQ, Persistent, "HTTP 401", 0, 0},
		{"402", 402, sentinel, noulQ, Persistent, "HTTP 402", 0, 0},
		{"403", 403, sentinel, noulQ, Persistent, "HTTP 403", 0, 0},
		{"404", 404, sentinel, noulQ, Persistent, "HTTP 404", 0, 0},
		{"415", 415, sentinel, noulQ, Persistent, "HTTP 415", 0, 0},
		{
			"422", 422, `{"detail":[{"msg":"` + sentinel + `"}]}`, noulQ, Persistent, "HTTP 422", 0,
			0,
		},
		{
			"body over 1 MiB", 200, noulBody(Pin, "0.5") + strings.Repeat(" ", MaxAnswer), noulQ,
			Persistent, CauseAnswerTooLong, 0, 0,
		},
		{"not JSON", 200, sentinel, noulQ, Persistent, CauseMalformed, 0, 0},
		{
			"no model", 200, `{"answers":{"q":{"type":"noul","noul":0.5}}}`, noulQ, Persistent,
			CauseMalformed, 0, 0,
		},
		{
			"null answers", 200, `{"model":"jev-1.14.0","answers":null}`, noulQ, Persistent,
			CauseMalformed, 0, 0,
		},
		{"other model", 200, noulBody("jev-latest", "0.5"), noulQ, Persistent, CauseModel, 0, 0},
		{
			"missing answer", 200, `{"model":"jev-1.14.0","answers":{}}`, noulQ, Persistent,
			CauseMissing, 0, 0,
		},
		{"null noul", 200, noulBody(Pin, "null"), noulQ, Persistent, CauseMissing, 0, 0},
		{
			"wrong type", 200, `{"model":"jev-1.14.0","answers":{"q":{"type":"choice"}}}`, noulQ,
			Persistent, CauseMissing, 0, 0,
		},
		{"string noul", 200, noulBody(Pin, `"0.5"`), noulQ, Persistent, CauseMalformed, 0, 0},
		{"above 1", 200, noulBody(Pin, "1.5"), noulQ, Persistent, CauseProbability, 0, 0},
		{"below 0", 200, noulBody(Pin, "-0.1"), noulQ, Persistent, CauseProbability, 0, 0},
		{"overflow", 200, noulBody(Pin, "1e999"), noulQ, Persistent, CauseMalformed, 0, 0},
		{
			"choice label unknown", 200, `{"model":"jev-1.14.0","answers":{"q":{"type":"choice",` +
				`"choice":"z","confidence":0.7,"probabilities":{"a":0.7,"b":0.3}}}}`, choiceQ,
			Persistent, CauseMissing, 0, 0,
		},
		{
			"choice probability missing", 200, `{"model":"jev-1.14.0","answers":{"q":{"type":` +
				`"choice","choice":"a","confidence":0.7,"probabilities":{"a":0.7}}}}`, choiceQ,
			Persistent, CauseMissing, 0, 0,
		},
		{
			"slow headers", 200, noulBody(Pin, "0.5"), noulQ, Deadline, CauseDeadline,
			2 * time.Second, 0,
		},
		{
			"slow body", 200, noulBody(Pin, "0.5"), noulQ, Deadline, CauseDeadline, 0,
			2 * time.Second,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits.Add(1)
					if r.Header.Get("Authorization") != "Bearer k" {
						t.Errorf("authorization %q", r.Header.Get("Authorization"))
					}
					select {
					case <-time.After(c.delay):
					case <-r.Context().Done():
						return
					}
					w.WriteHeader(c.status)
					half := len(c.body) / 2
					_, _ = w.Write([]byte(c.body[:half]))
					if c.bodyGap > 0 {
						if fl, ok := w.(http.Flusher); ok {
							fl.Flush()
						}
						select {
						case <-time.After(c.bodyGap):
						case <-r.Context().Done():
							return
						}
					}
					_, _ = w.Write([]byte(c.body[half:]))
				}),
			)
			defer srv.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
			defer cancel()
			o := Live{Endpoint: srv.URL, Key: "k"}.Send(ctx, testBatch(c.qs))
			if o.Class != c.class || o.Cause != c.cause {
				t.Fatalf("outcome = class %d cause %q, want %d %q", o.Class, o.Cause, c.class,
					c.cause)
			}
			if strings.Contains(fmt.Sprintf("%+v %s", o, o.Describe()), sentinel) {
				t.Fatal("the outcome echoes the body")
			}
			if n := hits.Load(); n != 1 {
				t.Fatalf("%d requests, want 1 (no retry)", n)
			}
		})
	}

	t.Run("network error", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()
		o := Live{Endpoint: url, Key: "k"}.Send(t.Context(), testBatch(noulQ))
		if o.Class != Transient || o.Cause != CauseNetwork {
			t.Fatalf("outcome = %+v", o)
		}
	})

	t.Run("client timeout inside the work deadline", func(t *testing.T) {
		done := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-done:
			}
		}))
		defer srv.Close()
		defer close(done)
		client := &http.Client{Timeout: 100 * time.Millisecond}
		o := Live{Endpoint: srv.URL, Key: "k", Client: client}.Send(t.Context(), testBatch(noulQ))
		if o.Class != Transient || o.Cause != CauseTimeout {
			t.Fatalf("outcome = %+v", o)
		}
	})
}

// gate is a transport whose batches finish in the reverse of their start order.
type gate struct {
	mu      sync.Mutex
	started []int
	release map[int]chan struct{}
}

func (g *gate) Send(ctx context.Context, b Batch) Outcome {
	i := int(b.Body[0])
	g.mu.Lock()
	g.started = append(g.started, i)
	ch := g.release[i]
	g.mu.Unlock()
	select {
	case <-ch:
	case <-ctx.Done():
		return Outcome{Class: Deadline, Cause: CauseDeadline}
	}
	return Outcome{Class: OK, Answers: map[string]Answer{"q": {
		Type: TypeNoul,
		Noul: float64(i) / 10,
	}}}
}

func TestOrder(t *testing.T) {
	g := &gate{release: map[int]chan struct{}{}}
	batches := make([]Batch, 0, 4)
	for i := range 4 {
		g.release[i] = make(chan struct{})
		batches = append(batches, Batch{Body: []byte{byte(i)}})
	}
	go func() {
		for i := 3; i >= 0; i-- {
			for {
				g.mu.Lock()
				n := len(g.started)
				g.mu.Unlock()
				if n == 4 {
					break
				}
				time.Sleep(time.Millisecond)
			}
			close(g.release[i])
			time.Sleep(5 * time.Millisecond)
		}
	}()
	out := Do(t.Context(), g, batches)
	for i, o := range out {
		if o.Answers["q"].Noul != float64(i)/10 {
			t.Fatalf("outcome %d = %+v: not in batch order", i, o)
		}
	}
}

func TestDoDeadline(t *testing.T) {
	g := &gate{release: map[int]chan struct{}{}}
	batches := make([]Batch, 0, InFlight+2)
	for i := range InFlight + 2 {
		g.release[i] = make(chan struct{})
		batches = append(batches, Batch{Body: []byte{byte(i)}})
	}
	close(g.release[0])
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	out := Do(ctx, g, batches)
	if out[0].Class != OK {
		t.Fatalf("the answered batch was lost: %+v", out[0])
	}
	for _, o := range out[1:] {
		if o.Class != Deadline {
			t.Fatalf("outcome %+v, want deadline", o)
		}
	}
	if len(g.started) > InFlight+1 {
		t.Fatalf("%d batches started", len(g.started))
	}
}
