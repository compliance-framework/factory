package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/compliance-framework/factory/internal/apiclient"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// commandsServer is POST /api/v1/commands: it records the command and answers with reply.
type commandsServer struct {
	mu   sync.Mutex
	auth []string
	cmds []apiclient.Command
}

func (c *commandsServer) handler(status int, reply string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/commands" {
			http.NotFound(w, r)
			return
		}
		var cmd apiclient.Command
		if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		c.auth = append(c.auth, r.Header.Get("Authorization"))
		c.cmds = append(c.cmds, cmd)
		c.mu.Unlock()
		ct := "application/json"
		if status >= 400 {
			ct = "application/problem+json"
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	})
}

func TestDispatch(t *testing.T) {
	event := filepath.Join("testdata", "serve", "dispatch-event.json")
	env := map[string]string{envCLIToken: testCLIToken}

	t.Run("posts the event as a dispatch command", func(t *testing.T) {
		cs := &commandsServer{}
		ts := httptest.NewServer(cs.handler(http.StatusAccepted, `{"type":"dispatch","event":"inject/req-1"}`))
		defer ts.Close()
		var out, errOut bytes.Buffer
		if code := runDispatch(context.Background(), []string{"--event", event, "--server", ts.URL + "/"}, getenvOf(env), &out, &errOut); code != 0 {
			t.Fatalf("exit %d: %s", code, errOut.String())
		}
		if out.String() != "inject/req-1\n" {
			t.Errorf("stdout %q", out.String())
		}
		if len(cs.cmds) != 1 {
			t.Fatalf("%d commands", len(cs.cmds))
		}
		cmd := cs.cmds[0]
		if cmd.Type != apiclient.CommandTypeDispatch || cmd.Target != nil || cmd.Args == nil || cmd.Args.Event == nil {
			t.Fatalf("command %+v", cmd)
		}
		if ev := cmd.Args.Event; ev.Source != "github-public" || ev.Type != v1alpha1.EventIssueOpened || ev.Data.Object == nil ||
			ev.Data.Object.Ref != "compliance-framework/docs#42" || ev.ID != "" {
			t.Errorf("event %+v", ev)
		}
		if cs.auth[0] != "Bearer "+testCLIToken {
			t.Errorf("authorization %q", cs.auth[0])
		}
	})

	t.Run("server from the environment", func(t *testing.T) {
		cs := &commandsServer{}
		ts := httptest.NewServer(cs.handler(http.StatusAccepted, `{"type":"dispatch","event":"inject/req-2"}`))
		defer ts.Close()
		var out, errOut bytes.Buffer
		e := map[string]string{envCLIToken: testCLIToken, envServer: ts.URL}
		if code := runDispatch(context.Background(), []string{"--event", event}, getenvOf(e), &out, &errOut); code != 0 || len(cs.cmds) != 1 {
			t.Fatalf("exit %d, %d commands: %s", code, len(cs.cmds), errOut.String())
		}
	})

	t.Run("a refused command", func(t *testing.T) {
		cs := &commandsServer{}
		ts := httptest.NewServer(cs.handler(http.StatusConflict,
			`{"type":"about:blank","title":"Conflict","status":409,"code":"conflict","detail":"event inject/x exists"}`))
		defer ts.Close()
		var out, errOut bytes.Buffer
		code := runDispatch(context.Background(), []string{"--event", event, "--server", ts.URL}, getenvOf(env), &out, &errOut)
		if code != 1 || !strings.Contains(errOut.String(), "409") || !strings.Contains(errOut.String(), "(conflict): event inject/x exists") {
			t.Fatalf("exit %d, stderr %q", code, errOut.String())
		}
		if strings.Contains(errOut.String(), testCLIToken) {
			t.Error("the token is in the error")
		}
	})

	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`{"source":"github-public","typo":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"no event", []string{}, env, "--event is required"},
		{"no token", []string{"--event", event}, map[string]string{}, envCLIToken + " is not set"},
		{"missing file", []string{"--event", "nope.json"}, env, "nope.json"},
		{"unknown field", []string{"--event", bad}, env, "unknown field"},
		{"bad server", []string{"--event", event, "--server", "localhost:8080"}, env, "--server"},
		{"extra args", []string{"--event", event, "x"}, env, "unexpected arguments"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := runDispatch(context.Background(), tt.args, getenvOf(tt.env), &out, &errOut); code != exitUsage || !strings.Contains(errOut.String(), tt.want) {
				t.Fatalf("exit %d, stderr %q, want %q", code, errOut.String(), tt.want)
			}
		})
	}
}
