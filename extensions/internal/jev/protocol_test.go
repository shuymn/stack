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
	req, err := ParseRequest([]byte(`{"stage":"outbound","jev":null,"extra":1}`))
	if err != nil || req.Stage != "outbound" || req.Jev != nil {
		t.Fatalf("%+v %v", req, err)
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
