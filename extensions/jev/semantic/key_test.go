package semantic

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/shuymn/stack/extensions/internal/jev"
)

// authServer records the Authorization header of every request and answers 0.1.
func authServer(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		_, _ = w.Write(answer(t, body, map[string]float64{MetaDuty: 0.1, Unsupported: 0.1}))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), auths...)
	}
}

// putKey writes the key file under home, or a directory there when dir is set.
func putKey(t *testing.T, home, content string, dir bool) {
	t.Helper()
	p := jev.KeyPath(home)
	if dir {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func noEnv(string) string { return "" }

// TestKeyFile covers the key lookup of a live request (§3.1).
func TestKeyFile(t *testing.T) {
	r := stateRepo(t)

	t.Run("file key when the variable is unset", func(t *testing.T) {
		srv, auths := authServer(t)
		env := repoEnv(r)
		env.Getenv, env.Endpoint, env.Home = noEnv, srv.URL, t.TempDir()
		putKey(t, env.Home, "file-key\n", false)
		res := runMain(t, env, outboundReq(r, "t", "b"))
		got := auths()
		if res.resp.Status != jev.StatusPass || len(got) == 0 || got[0] != "Bearer file-key" {
			t.Fatalf("response %+v, auth %q", res.resp, got)
		}
	})

	t.Run("variable first, file never read", func(t *testing.T) {
		srv, auths := authServer(t)
		env := repoEnv(r)
		env.Endpoint, env.Home = srv.URL, t.TempDir()
		putKey(t, env.Home, "", true)
		res := runMain(t, env, outboundReq(r, "t", "b"))
		got := auths()
		if res.resp.Status != jev.StatusPass || len(got) == 0 || got[0] != "Bearer k" {
			t.Fatalf("response %+v, auth %q", res.resp, got)
		}
	})

	t.Run("unusable file warns with no request", func(t *testing.T) {
		srv, auths := authServer(t)
		env := repoEnv(r)
		env.Getenv, env.Endpoint, env.Home = noEnv, srv.URL, t.TempDir()
		putKey(t, env.Home, " \n", false)
		res := runMain(t, env, outboundReq(r, "t", "b"))
		want := "jev-semantic: key file " + jev.KeyDisplay + " is blank: " +
			"write the key into it, or remove it to opt out"
		if res.code != 0 || res.resp.Status != jev.StatusWarn || len(res.resp.Findings) != 1 ||
			res.resp.Findings[0].Kind != jev.KindError ||
			res.resp.Findings[0].Detail != "key file is blank" || res.resp.Context != want {
			t.Fatalf("response %+v", res.resp)
		}
		if n := len(auths()); n != 0 {
			t.Fatalf("%d requests sent", n)
		}
		if _, err := os.Stat(jev.LedgerPath(env.Home)); !os.IsNotExist(err) {
			t.Fatalf("ledger written: %v", err)
		}
	})

	t.Run("replay never looks the key up", func(t *testing.T) {
		env := repoEnv(r)
		env.Getenv, env.Home = noEnv, t.TempDir()
		putKey(t, env.Home, " \n", false)
		req := outboundReq(r, "t", "b")
		req["jev"] = map[string]any{"records": map[string]any{}}
		res := runMain(t, env, req)
		for _, f := range res.resp.Findings {
			if f.Kind == jev.KindError || strings.Contains(f.Detail, "key file") {
				t.Fatalf("replay looked the key up: %+v", res.resp)
			}
		}
		if len(res.resp.Findings) == 0 || res.resp.Findings[0].Kind != jev.KindUnrecorded {
			t.Fatalf("response %+v, want jev-unrecorded", res.resp)
		}
	})
}
