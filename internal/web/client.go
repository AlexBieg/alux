package web

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

const (
	fetchTimeout   = 30 * time.Second
	dialTimeout    = 10 * time.Second
	headerTimeout  = 15 * time.Second
	maxRedirects   = 10
	maxPageBytes   = 5 << 20
	fetchUserAgent = "alux/1.0 (+https://github.com/alexbieg/alux; research fetcher)"
)

// Ranges net.Addr's own predicates do not name: the shared-address space a carrier NAT hands
// out, "this network", and the reserved fourth of IPv4 the broadcast address sits in.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

// hardenedClient is the only way this package reaches a page: no proxy, because a proxied dial
// would hand the guard an address the request never goes to.
func hardenedClient(control func(network, address string, c syscall.RawConn) error) *http.Client {
	dialer := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second, Control: control}
	return &http.Client{
		Timeout: fetchTimeout,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConnsPerHost:   2,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   dialTimeout,
			ResponseHeaderTimeout: headerTimeout,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			return nil
		},
	}
}

// guard runs on the resolved address immediately before connect, which is the one point a name
// that answers publicly and then rebinds to a private host cannot slip past. Every redirect hop
// dials again, so every hop is checked.
func (f *Fetcher) guard(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%s: %w", address, ErrBlocked)
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("%s: %w", host, ErrBlocked)
	}
	addr = addr.Unmap()
	if f.allowLoopback && addr.IsLoopback() {
		return nil
	}
	if !routable(addr) {
		return fmt.Errorf("%s: %w", addr, ErrBlocked)
	}
	return nil
}

// routable reports whether an address is one the public internet could have answered with. The
// caller unmaps first: ::ffff:10.0.0.1 is private, and none of these predicates say so.
func routable(addr netip.Addr) bool {
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsLoopback() || addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsMulticast() || addr.IsInterfaceLocalMulticast() {
		return false
	}
	for _, prefix := range blockedPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}
