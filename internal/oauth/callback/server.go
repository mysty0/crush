// Package callback runs a loopback HTTP server that receives the redirect
// at the end of an OAuth 2.0 authorization-code flow. The provider sends
// the user's browser to a localhost URL with the authorization code (and
// state) as query parameters; this server captures them and hands them
// back to the login flow.
package callback

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// defaultSuccessHTML is shown in the browser after a successful redirect.
const defaultSuccessHTML = `<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>Crush</title>
<style>body{font-family:system-ui,sans-serif;background:#1a1a2e;color:#eee;
display:flex;align-items:center;justify-content:center;height:100vh;margin:0}
.card{text-align:center}h1{color:#ff5fd2}p{color:#aaa}</style></head>
<body><div class="card"><h1>&#128156; Authentication complete</h1>
<p>You can close this tab and return to Crush.</p></div></body></html>`

// Result carries the parsed query parameters from the redirect request.
type Result struct {
	// Code is the OAuth authorization code (the "code" query parameter).
	Code string
	// State is the anti-CSRF state echoed back by the provider.
	State string
	// Error is the "error" query parameter when the provider reports a
	// failure instead of a code (e.g. "access_denied").
	Error string
	// ErrorDescription is the human-readable "error_description" if any.
	ErrorDescription string
}

// Config configures a callback Server.
type Config struct {
	// Port is the preferred loopback port to bind. When zero (or when
	// AllowPortFallback is set and the port is busy) an ephemeral port is
	// chosen. Providers that pin an exact redirect URI (e.g. OpenAI Codex
	// only allows localhost:1455) must set a fixed Port with
	// AllowPortFallback=false.
	Port int
	// AllowPortFallback lets the server pick an ephemeral port when the
	// preferred Port is unavailable. Leave false when the redirect URI is
	// registered with the provider and must match exactly.
	AllowPortFallback bool
	// Path is the callback path, e.g. "/auth/callback". Defaults to "/".
	Path string
	// Hostname is the redirect host, "localhost" or "127.0.0.1". Defaults
	// to "localhost".
	Hostname string
	// SuccessHTML overrides the page shown to the user on success.
	SuccessHTML string
}

// Server is a running loopback callback listener. Callers must Close it.
type Server struct {
	srv      *http.Server
	listener net.Listener
	port     int
	path     string
	hostname string
	results  chan Result
}

// Start binds the callback listener and begins serving. It returns a Server
// whose RedirectURI reflects the actually-bound port (which may differ from
// the requested one when AllowPortFallback is set).
func Start(cfg Config) (*Server, error) {
	path := cfg.Path
	if path == "" {
		path = "/"
	}
	hostname := cfg.Hostname
	if hostname == "" {
		hostname = "localhost"
	}
	successHTML := cfg.SuccessHTML
	if successHTML == "" {
		successHTML = defaultSuccessHTML
	}

	listener, port, err := bindListener(cfg.Port, cfg.AllowPortFallback)
	if err != nil {
		return nil, err
	}

	s := &Server{
		listener: listener,
		port:     port,
		path:     path,
		hostname: hostname,
		results:  make(chan Result, 1),
	}

	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		res := Result{
			Code:             q.Get("code"),
			State:            q.Get("state"),
			Error:            q.Get("error"),
			ErrorDescription: q.Get("error_description"),
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if res.Error != "" || res.Code == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, "<html><body><h1>Authentication failed</h1><p>%s</p></body></html>",
				htmlEscape(firstNonEmpty(res.ErrorDescription, res.Error, "no authorization code received")))
		} else {
			_, _ = w.Write([]byte(successHTML))
		}
		// Deliver at most once; ignore duplicate hits (favicon, retries).
		select {
		case s.results <- res:
		default:
		}
	})

	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.srv.Serve(listener) }()
	return s, nil
}

// bindListener binds the preferred port, optionally falling back to an
// ephemeral one, and reports the port that was actually bound.
func bindListener(port int, allowFallback bool) (net.Listener, int, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		if port != 0 && allowFallback {
			listener, err = net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				return nil, 0, fmt.Errorf("bind callback listener: %w", err)
			}
		} else {
			return nil, 0, fmt.Errorf("bind callback listener on port %d: %w", port, err)
		}
	}
	return listener, listener.Addr().(*net.TCPAddr).Port, nil
}

// RedirectURI returns the loopback URL the provider should redirect to.
func (s *Server) RedirectURI() string {
	return fmt.Sprintf("http://%s:%d%s", s.hostname, s.port, s.path)
}

// Port returns the port the callback server is listening on.
func (s *Server) Port() int { return s.port }

// Wait blocks until the browser hits the callback endpoint, the context is
// cancelled, or manual is delivered on the returned channel first. It
// returns the captured Result. Callers that also accept a pasted code
// should select on Wait and their own manual-input channel.
func (s *Server) Wait(ctx context.Context) (Result, error) {
	select {
	case <-ctx.Done():
		return Result{}, ctx.Err()
	case res := <-s.results:
		if res.Error != "" {
			return res, fmt.Errorf("authorization failed: %s", firstNonEmpty(res.ErrorDescription, res.Error))
		}
		if res.Code == "" {
			return res, errors.New("no authorization code received")
		}
		return res, nil
	}
}

// Close shuts down the callback server and releases the port.
func (s *Server) Close() error {
	if s.srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.srv.Shutdown(ctx)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func htmlEscape(s string) string {
	// Minimal escaping for the tiny error page; the value comes from the
	// provider's error_description query parameter.
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch r {
		case '<':
			out = append(out, []rune("&lt;")...)
		case '>':
			out = append(out, []rune("&gt;")...)
		case '&':
			out = append(out, []rune("&amp;")...)
		default:
			out = append(out, r)
		}
	}
	return string(out)
}
