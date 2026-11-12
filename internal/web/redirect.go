package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// SameHostRedirects is an http.Client CheckRedirect for clients that carry credentials: it refuses
// a hop to any host (hostname and port) other than the first request's.
func SameHostRedirects(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if from := via[0].URL.Host; !strings.EqualFold(req.URL.Host, from) {
		return fmt.Errorf("refusing redirect from %s to another host %s", from, req.URL.Host)
	}
	return nil
}
