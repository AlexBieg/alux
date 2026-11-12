package mcpclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/alexbieg/alux/internal/web"
	"github.com/modelcontextprotocol/go-sdk/auth"
)

const (
	// Fixed so the redirect URI registered with each server stays valid across runs.
	callbackPort  = 47821
	signInTimeout = 5 * time.Minute
	callbackPath  = "/callback"
)

var redirectURL = fmt.Sprintf("http://127.0.0.1:%d%s", callbackPort, callbackPath)

// Browsers ignore window.close() on a tab whose history holds pages the user clicked through,
// so the text stays for the sign-ins where it cannot close.
const signedInPage = `<!doctype html><title>Signed in</title><p>Signed in. You can close this tab.</p><script>window.close()</script>`

// Remote connections take turns: a sign-in holds the shared callback port until it finishes, so
// two servers needing one at the same time would otherwise collide on the port.
var signInTurn sync.Mutex

var oauthClient = &http.Client{CheckRedirect: web.SameHostRedirects}

func (m *Manager) authHandler(server, resource string) (auth.OAuthHandler, error) {
	handler, _, err := newHandler(handlerOpts{
		Dir:      m.authDir(),
		Server:   server,
		Resource: resource,
		Redirect: redirectURL,
		Guard:    true,
		HTTP:     oauthClient,
		OnBrowser: func() {
			m.say("mcp: %s needs sign-in, opening browser", server)
		},
		// Said out loud because the alternative is a browser opening for no visible reason. The
		// scope names are the useful part: they are what the server added since this client
		// registered, which is also what it is about to become able to do.
		OnStale: func(missing []string) {
			m.say("mcp: %s added %s since sign-in, re-registering", server, strings.Join(missing, " "))
		},
		Fetch: m.fetchCode,
	})
	return handler, err
}

// fetchCode opens the browser and waits for the authorization server to redirect back.
//
// The listener is claimed *before* the browser opens. A redirect that arrives while another
// alux happens to be listening lands on that alux, where it fails the state check and
// breaks a sign-in that was going fine.
func (m *Manager) fetchCode(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
	signInTurn.Lock()
	defer signInTurn.Unlock()

	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", callbackPort))
	if err != nil {
		return nil, fmt.Errorf("another alux is signing in (callback port %d is busy) — retry once it finishes", callbackPort)
	}

	result := make(chan *auth.AuthorizationResult, 1)
	failure := make(chan error, 1)

	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != callbackPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		q := r.URL.Query()
		code := q.Get("code")
		if code == "" {
			message := q.Get("error_description")
			if message == "" {
				message = "Sign-in failed."
			}
			http.Error(w, message, http.StatusBadRequest)
			failure <- errors.New(message)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(signedInPage))
		result <- &auth.AuthorizationResult{Code: code, State: q.Get("state"), Iss: q.Get("iss")}
	})}

	go func() { _ = server.Serve(listener) }()
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()

	openBrowser(args.URL)

	select {
	case got := <-result:
		return got, nil
	case err := <-failure:
		return nil, err
	case <-time.After(signInTimeout):
		return nil, errors.New("sign-in timed out")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Fire and forget: a browser that will not open is reported by the sign-in timing out, and
// there is nothing useful to do with the error here.
func openBrowser(target string) {
	opener := os.Getenv("BROWSER")
	if opener == "" {
		opener = "open"
	}
	if _, err := url.Parse(target); err != nil {
		return
	}
	cmd := exec.Command(opener, target)
	_ = cmd.Start()
	go func() { _ = cmd.Wait() }()
}
