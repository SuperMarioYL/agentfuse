package proxy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/SuperMarioYL/agentfuse/internal/account"
	"github.com/SuperMarioYL/agentfuse/internal/budget"
	"github.com/SuperMarioYL/agentfuse/internal/ledger"
)

// Server hosts both the Anthropic and OpenAI handlers under one local listener.
type Server struct {
	cfg         *budget.Config
	projectRoot string
	led         *ledger.Ledger
	accounts    *account.File

	httpSrv  *http.Server
	listener net.Listener
	wg       sync.WaitGroup
}

// New constructs a server. led, cfg, projectRoot are required.
func New(projectRoot string, cfg *budget.Config, led *ledger.Ledger, accts *account.File) *Server {
	if accts == nil {
		accts = &account.File{Accounts: map[string]account.Account{}}
	}
	return &Server{
		cfg:         cfg,
		projectRoot: projectRoot,
		led:         led,
		accounts:    accts,
	}
}

// Start binds 127.0.0.1:0 and serves until Stop is called.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("bind proxy listener: %w", err)
	}
	s.listener = ln

	mux := http.NewServeMux()
	mux.Handle("/anthropic/", http.StripPrefix("/anthropic", anthropicHandler(s)))
	mux.Handle("/openai/", http.StripPrefix("/openai", openaiHandler(s)))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	s.httpSrv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		_ = s.httpSrv.Serve(ln)
	}()
	return nil
}

// Addr returns the bound 127.0.0.1:PORT.
func (s *Server) Addr() string {
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// AnthropicBaseURL returns the URL to set ANTHROPIC_BASE_URL to.
func (s *Server) AnthropicBaseURL() string { return "http://" + s.Addr() + "/anthropic" }

// OpenAIBaseURL returns the URL to set OPENAI_BASE_URL to.
func (s *Server) OpenAIBaseURL() string { return "http://" + s.Addr() + "/openai" }

// Stop gracefully shuts down. Safe to call after Start failed.
func (s *Server) Stop(ctx context.Context) error {
	if s.httpSrv == nil {
		return nil
	}
	err := s.httpSrv.Shutdown(ctx)
	s.wg.Wait()
	return err
}
