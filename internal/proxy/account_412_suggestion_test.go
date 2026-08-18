package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/SuperMarioYL/agentfuse/internal/account"
	"github.com/SuperMarioYL/agentfuse/internal/budget"
)

// TestAccountNotFoundSuggestsFuseInit is the v0.11 regression for
// fix-account-412-suggests-nonexistent-run-flag: when a named account is
// configured (.fuse.toml's account field) but absent from accounts.toml, every
// handler must return HTTP 412 with a suggested_command pointing at
// `fuse init --account <name>` — a REAL fuse flag — NOT the non-existent
// `fuse run --account <name>` (run registers no --account flag, and after
// fix-run-passthrough-flags-unknown any --token after `run` is forwarded
// verbatim to the child, so the old remediation sent users down a path that
// either errored "unknown flag" or pushed --account into the agent CLI).
func TestAccountNotFoundSuggestsFuseInit(t *testing.T) {
	cases := []struct {
		name string
		url  string
		body string
	}{
		{
			name: "anthropic",
			url:  "/anthropic/v1/messages",
			body: `{"model":"claude-sonnet-4","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "openai",
			url:  "/openai/v1/chat/completions",
			body: `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "deepseek",
			url:  "/deepseek/v1/chat/completions",
			body: `{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "gemini",
			url:  "/gemini/v1beta/models/gemini-1.5-pro:generateContent",
			body: `{"contents":[{"parts":[{"text":"hi"}]}]}`,
		},
		{
			name: "openai_compat",
			url:  "/openai_compat/v1/chat/completions",
			body: `{"model":"llama-3.1-70b","messages":[{"role":"user","content":"hi"}]}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			led := mustOpenLedger(t)
			cfg := &budget.Config{
				CapUSD:   5.00,
				Window:   "project",
				Account:  "ghost", // not present in the accounts file below
				Provider: c.name,
				// openai_compat gates on upstream_url BEFORE the account guard;
				// set a dummy so we reach the account-not-found 412 branch
				// instead of the upstream_url-missing 412.
				UpstreamURL: "http://127.0.0.1:1",
			}
			s := New("/proj/412-"+c.name, cfg, led,
				&account.File{Accounts: map[string]account.Account{}})
			if err := s.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = s.Stop(ctx)
			}()

			req, _ := http.NewRequest(http.MethodPost, "http://"+s.Addr()+c.url,
				bytes.NewReader([]byte(c.body)))
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusPreconditionFailed {
				t.Fatalf("%s: expected 412, got %d", c.name, resp.StatusCode)
			}
			var fb struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
				Suggested string `json:"suggested_command"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&fb); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(fb.Suggested, "fuse init --account") {
				t.Fatalf("%s: suggested_command %q does not mention `fuse init --account`",
					c.name, fb.Suggested)
			}
			if strings.Contains(fb.Suggested, "fuse run --account") {
				t.Fatalf("%s: suggested_command still points at non-existent `fuse run --account`: %q",
					c.name, fb.Suggested)
			}
		})
	}
}
