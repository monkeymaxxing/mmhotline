package app

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

var errAddressNotPublic = errors.New("address is not a public internet address")

// publicInternetClient is the HTTP client for webhook addresses customers
// choose. It never follows redirects, verifies TLS certificates, and, unless
// talking to a development fake, refuses to connect
// to anything but a public internet address. The address is checked at the
// moment of connecting, so a second DNS answer cannot redirect a request.
func publicInternetClient(fake bool, timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: timeout}
	if !fake {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return errAddressNotPublic
			}
			ip, err := netip.ParseAddr(host)
			if err != nil || !publicInternetAddress(ip) {
				return errAddressNotPublic
			}
			return nil
		}
	}
	return &http.Client{
		Transport:     &http.Transport{DialContext: dialer.DialContext, Proxy: nil, TLSHandshakeTimeout: timeout, ResponseHeaderTimeout: timeout, MaxResponseHeaderBytes: 64 << 10, MaxIdleConns: 1, IdleConnTimeout: time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

var notPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved and broadcast
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64, which can reach private IPv4
	netip.MustParsePrefix("64:ff9b:1::/48"),  // local NAT64
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("2002::/16"),       // 6to4, which embeds IPv4
	netip.MustParsePrefix("2001::/32"),       // Teredo, which embeds IPv4
	netip.MustParsePrefix("3fff::/20"),       // documentation
}

// globalUnicastIPv6 is the only IPv6 space ordinary internet hosts use.
var globalUnicastIPv6 = netip.MustParsePrefix("2000::/3")

// publicInternetAddress reports whether ip is an ordinary public unicast
// address: not loopback, private, link-local (which includes cloud metadata
// at 169.254.169.254), multicast, unspecified, or a special range. IPv4
// addresses written in IPv6 form are checked as IPv4.
func publicInternetAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.Zone() != "" || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	// This also refuses IPv4-compatible (::a.b.c.d) and IPv4-translated forms.
	if ip.Is6() && !globalUnicastIPv6.Contains(ip) {
		return false
	}
	for _, p := range notPublicPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}
