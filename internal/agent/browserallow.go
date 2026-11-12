package agent

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/alexbieg/alux/internal/paths"
)

const browserDomainsFile = paths.Config + "/browser-domains.json"

// browserOnce is the hosts allowed once, which last until alux exits whichever session asked.
var browserOnce = struct {
	sync.Mutex
	set map[string]bool
}{set: map[string]bool{}}

func normalHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	return strings.TrimSuffix(host, ".")
}

func isLocal(host string) bool {
	switch normalHost(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

func hostAllowed(host string, entries []string) bool {
	host = normalHost(host)
	if host == "" {
		return false
	}
	for _, entry := range entries {
		entry = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(entry), "."))
		if parent, wild := strings.CutPrefix(entry, "*."); wild {
			if parent != "" && strings.HasSuffix(host, "."+parent) {
				return true
			}
			continue
		}
		if entry != "" && normalHost(entry) == host {
			return true
		}
	}
	return false
}

// checkURL returns the host a URL needs allowing, or "" when it needs nothing.
func checkURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "about:blank" {
		return "", nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%q is not a URL", raw)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		host := normalHost(parsed.Hostname())
		if host == "" {
			return "", fmt.Errorf("%q has no host", raw)
		}
		if isLocal(host) {
			return "", nil
		}
		return host, nil
	case "file":
		if parsed.Host != "" && parsed.Host != "localhost" {
			return "", fmt.Errorf("file URLs on another host are refused")
		}
		clean := filepath.Clean(parsed.Path)
		if strings.HasPrefix(clean, "/tmp/") || strings.HasPrefix(clean, "/private/tmp/") {
			return "", nil
		}
		return "", fmt.Errorf("file URLs are allowed only under /tmp, and %s is not", clean)
	}
	return "", fmt.Errorf("only http, https, about:blank and file URLs under /tmp can be opened, not %q", raw)
}

// wildcardParent is the *.parent entry offered beside an exact host, or "" when there is no sensible one.
func wildcardParent(host string) string {
	if net.ParseIP(host) != nil {
		return ""
	}
	_, parent, found := strings.Cut(host, ".")
	if !found || !strings.Contains(parent, ".") {
		return ""
	}
	return "*." + parent
}

func browserDomainsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, browserDomainsFile), nil
}

func savedBrowserDomains() []string {
	path, err := browserDomainsPath()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var file struct {
		Allow []string `json:"allow"`
	}
	if json.Unmarshal(data, &file) != nil {
		return nil
	}
	return file.Allow
}

func saveBrowserDomain(entry string) error {
	path, err := browserDomainsPath()
	if err != nil {
		return err
	}
	entries := savedBrowserDomains()
	if slices.Contains(entries, entry) {
		return nil
	}
	data, err := json.MarshalIndent(struct {
		Allow []string `json:"allow"`
	}{append(entries, entry)}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".browser-domains-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func allowBrowserOnce(host string) {
	browserOnce.Lock()
	defer browserOnce.Unlock()
	browserOnce.set[host] = true
}

func browserAllowlist(settings []string) []string {
	entries := append(slices.Clone(settings), savedBrowserDomains()...)
	browserOnce.Lock()
	defer browserOnce.Unlock()
	for host := range browserOnce.set {
		entries = append(entries, host)
	}
	return entries
}
