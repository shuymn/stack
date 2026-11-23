package jev

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestResponseBound(t *testing.T) {
	r := &Reply{}
	for i := range 20000 {
		r.Warn(Finding{
			Kind: "jev-meta-duty", Where: strings.Repeat("w", 40),
			Detail: fmt.Sprintf("finding %d", i),
		}, strings.Repeat("line ", 300))
	}
	resp := r.Response()
	var b bytes.Buffer
	if err := Write(&b, resp); err != nil {
		t.Fatal(err)
	}
	if b.Len() > MaxResponse {
		t.Fatalf("response of %d bytes over %d", b.Len(), MaxResponse)
	}
	last := resp.Findings[len(resp.Findings)-1]
	if last.Kind != KindOmitted || !strings.HasSuffix(last.Detail, "findings omitted") {
		t.Fatalf("last finding %+v", last)
	}
	if !strings.HasSuffix(resp.Context, "more lines omitted]") {
		t.Fatalf("context does not end with the marker: %q", resp.Context[len(resp.Context)-80:])
	}
	for l := range strings.SplitSeq(resp.Context, "\n") {
		if len(l) > maxLine {
			t.Fatalf("a context line of %d bytes", len(l))
		}
	}
	var decoded Response
	if err := json.Unmarshal(b.Bytes(), &decoded); err != nil || decoded.Status != StatusWarn {
		t.Fatalf("decoded %v (%v)", decoded.Status, err)
	}
}

func TestParseRequest(t *testing.T) {
	for _, bad := range []string{"", "{", "[]", `"x"`, `{"stage":1}`, `{}`, `{"stage":"a"} {}`} {
		if _, err := ParseRequest([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	req, err := ParseRequest([]byte(`{"stage":"outbound","extra":1}`))
	if err != nil || req.Stage != "outbound" || req.Jev != nil {
		t.Fatalf("%+v %v", req, err)
	}
	// An explicit null is kept, so it is an invalid replay rather than a live request.
	req, err = ParseRequest([]byte(`{"stage":"outbound","jev":null}`))
	if err != nil || req.Jev == nil {
		t.Fatalf("%+v %v", req, err)
	}
	if _, err := ParseReplay(req.Jev); err == nil {
		t.Fatal("a null jev object parsed as replay records")
	}
	req, err = ParseRequest([]byte(`{"stage":"outbound","jev":{"records":{}}}`))
	if err != nil || req.Jev == nil {
		t.Fatalf("%+v %v", req, err)
	}
}

func TestParseReplay(t *testing.T) {
	h := Hash([]byte("x"))
	for _, bad := range []string{
		`[]`, `{}`, `{"records":[]}`,
		`{"records":{"md5:1":{"status":400}}}`,
		`{"records":{"` + h + `":{}}}`,
		`{"records":{"` + h + `":{"status":200}}}`,
		`{"records":{"` + h + `":{"status":400,"answer":{}}}}`,
	} {
		if _, err := ParseReplay(json.RawMessage(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	r, err := ParseReplay(json.RawMessage(`{"records":{"` + h +
		`":{"status":400,"error":"max_tokens_exceeded"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if o := r.Send(t.Context(), Batch{Hash: h}); o.Class != TooLarge {
		t.Fatalf("outcome %+v", o)
	}
	if o := r.Send(t.Context(), Batch{Hash: Hash([]byte("y"))}); o.Class != Unrecorded {
		t.Fatalf("outcome %+v", o)
	}
}

// TestSetRecords: replacing the records keeps the request's other fields and the other keys of
// its jev object (§3.4).
func TestSetRecords(t *testing.T) {
	f := Fixture{Request: json.RawMessage(`{"stage":"cycle","jev":{"now":"2026-01-08T00:00:00Z",` +
		`"observe":"obs.json","records":{"sha256:` + strings.Repeat("a", 64) + `":{"status":500}}}}`)}
	h := "sha256:" + strings.Repeat("b", 64)
	if err := f.SetRecords(map[string]Record{h: {Status: 429}}); err != nil {
		t.Fatal(err)
	}
	var req struct {
		Stage string                     `json:"stage"`
		Jev   map[string]json.RawMessage `json:"jev"`
	}
	if err := json.Unmarshal(f.Request, &req); err != nil {
		t.Fatal(err)
	}
	if req.Stage != "cycle" || string(req.Jev["now"]) != `"2026-01-08T00:00:00Z"` ||
		string(req.Jev["observe"]) != `"obs.json"` {
		t.Fatalf("request %s", f.Request)
	}
	replay, err := f.RequestRecords()
	if err != nil || len(replay.Records) != 1 || replay.Records[h].Status != 429 {
		t.Fatalf("records %+v (%v)", replay.Records, err)
	}
}
