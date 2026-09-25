package webfetch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// privateIPNetworks holds pre-parsed private/reserved IP ranges.
// Populated in init() to avoid repeated CIDR parsing on every check.
var privateIPNetworks []*net.IPNet

func init() {
	cidrs := []string{
		"10.0.0.0/8",         // Class A private
		"172.16.0.0/12",      // Class B private
		"192.168.0.0/16",     // Class C private
		"127.0.0.0/8",        // Loopback
		"169.254.0.0/16",     // Link-local (cloud metadata 169.254.169.254)
		"0.0.0.0/8",          // Current network
		"100.64.0.0/10",      // Shared address space (CGN)
		"192.0.0.0/24",       // IETF Protocol
		"192.0.2.0/24",       // TEST-NET-1
		"198.51.100.0/24",    // TEST-NET-2
		"203.0.113.0/24",     // TEST-NET-3
		"224.0.0.0/4",        // Multicast
		"240.0.0.0/4",        // Reserved
		"255.255.255.255/32", // Broadcast
		// IPv6
		"::1/128",    // IPv6 loopback
		"fc00::/7",   // IPv6 unique local
		"fe80::/10",  // IPv6 link-local
		"::/128",     // IPv6 unspecified
	}
	privateIPNetworks = make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			panic(fmt.Sprintf("invalid CIDR in privateIPNetworks: %q: %v", cidr, err))
		}
		privateIPNetworks = append(privateIPNetworks, network)
	}
}

// SecurityChecker provides URL safety checks (SSRF, domain blocklist, scheme).
type SecurityChecker struct {
	config          WebFetchConfig
	blockedDomains  map[string]bool
	preApprovedSet  map[string]bool   // pure domain entries for O(1) lookup
	preApprovedList []string          // all entries (domain + path) for full matching
	pathEntries     []preApprovedPath // entries with path component
}

// preApprovedPath is a pre-approved entry with a path component.
type preApprovedPath struct {
	host string // domain part (e.g. "github.com")
	path string // path prefix (e.g. "/anthropics")
}

// NewSecurityChecker creates a SecurityChecker from config.
func NewSecurityChecker(config WebFetchConfig) *SecurityChecker {
	blocked := make(map[string]bool, len(config.BlockedDomains))
	for _, d := range config.BlockedDomains {
		blocked[strings.ToLower(d)] = true
	}
	preApprovedSet := make(map[string]bool, len(config.PreApprovedDomains))
	var pathEntries []preApprovedPath
	for _, entry := range config.PreApprovedDomains {
		slash := strings.Index(entry, "/")
		if slash == -1 {
			preApprovedSet[strings.ToLower(entry)] = true
		} else {
			pathEntries = append(pathEntries, preApprovedPath{
				host: strings.ToLower(entry[:slash]),
				path: entry[slash:],
			})
		}
	}
	return &SecurityChecker{
		config:          config,
		blockedDomains:  blocked,
		preApprovedSet:  preApprovedSet,
		preApprovedList: config.PreApprovedDomains,
		pathEntries:     pathEntries,
	}
}

// CheckURL performs a full URL safety check.
// Accepts context for DNS resolution timeout/cancellation.
func (s *SecurityChecker) CheckURL(ctx context.Context, rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	// Scheme check: only HTTP/HTTPS.
	scheme := strings.ToLower(parsed.Scheme)
	if !s.isAllowedScheme(scheme) {
		return fmt.Errorf("scheme %q not allowed, only HTTP/HTTPS permitted", parsed.Scheme)
	}

	// Credential check: no user:pass@host.
	if parsed.User != nil {
		return fmt.Errorf("URL must not contain credentials (username/password)")
	}

	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return fmt.Errorf("URL must contain a valid hostname")
	}

	// Blocklist check.
	if s.isBlocked(host) {
		return fmt.Errorf("%w: %q", ErrDomainBlocked, host)
	}

	// SSRF check: DNS resolve and verify IP is not private.
	if err := s.checkSSRF(ctx, host); err != nil {
		return fmt.Errorf("%w: %w", ErrSSRFBlocked, err)
	}

	return nil
}

// checkSSRF prevents Server-Side Request Forgery by resolving DNS and verifying
// all returned IPs are public (not private/reserved).
func (s *SecurityChecker) checkSSRF(ctx context.Context, host string) error {
	resolver := &net.Resolver{PreferGo: true}
	addrs, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDNSFailed, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("%w: no IP addresses resolved for %q", ErrDNSFailed, host)
	}
	for _, addr := range addrs {
		if isPrivateIP(addr.IP) {
			return fmt.Errorf("%w: resolves to private IP %s for %q", ErrSSRFBlocked, addr.IP.String(), host)
		}
	}
	return nil
}

// isPrivateIP checks whether ip falls within any private/reserved range.
func isPrivateIP(ip net.IP) bool {
	for _, network := range privateIPNetworks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// IsPreApproved checks whether host matches a pre-approved domain entry.
// Uses O(1) hash lookup for pure domain entries, then checks subdomains
// against path entries' domain parts.
func (s *SecurityChecker) IsPreApproved(host string) bool {
	host = strings.ToLower(host)
	// O(1) exact match for pure domain entries.
	if s.preApprovedSet[host] {
		return true
	}
	// Subdomain check: walk up parent domains.
	parts := strings.Split(host, ".")
	for i := 1; i < len(parts); i++ {
		parent := strings.Join(parts[i:], ".")
		if s.preApprovedSet[parent] {
			return true
		}
	}
	// Check path entries' domain parts (for entries like "github.com/anthropics").
	for _, pe := range s.pathEntries {
		if host == pe.host || strings.HasSuffix(host, "."+pe.host) {
			return true
		}
	}
	return false
}

// IsPreApprovedWithPath checks host+path against pre-approved entries with
// path-segment-boundary matching (e.g. "/anthropics" does NOT match "/anthropics-evil").
func (s *SecurityChecker) IsPreApprovedWithPath(host, path string) bool {
	host = strings.ToLower(host)
	// Pure domain entries: match host or subdomain.
	if s.preApprovedSet[host] {
		return true
	}
	parts := strings.Split(host, ".")
	for i := 1; i < len(parts); i++ {
		parent := strings.Join(parts[i:], ".")
		if s.preApprovedSet[parent] {
			return true
		}
	}
	// Path entries: match domain + path prefix with segment boundary.
	for _, pe := range s.pathEntries {
		if host == pe.host || strings.HasSuffix(host, "."+pe.host) {
			if path == pe.path || strings.HasPrefix(path, pe.path+"/") {
				return true
			}
		}
	}
	return false
}

func (s *SecurityChecker) isAllowedScheme(scheme string) bool {
	for _, allowed := range s.config.AllowedSchemes {
		if scheme == allowed {
			return true
		}
	}
	return false
}

func (s *SecurityChecker) isBlocked(host string) bool {
	host = strings.ToLower(host)
	if s.blockedDomains[host] {
		return true
	}
	parts := strings.Split(host, ".")
	for i := 1; i < len(parts)-1; i++ {
		parent := strings.Join(parts[i:], ".")
		if s.blockedDomains[parent] {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// safeNetDialer — prevents DNS rebinding (TOCTOU) by verifying IPs at
// connect time inside the http.Transport's DialContext.
// ---------------------------------------------------------------------------

// safeNetDialer wraps net.Dialer to validate resolved IPs before connecting.
//
// IMPORTANT: do NOT wrap the returned net.Conn — Go's http.Transport uses the
// request URL hostname for TLS SNI and certificate verification, not the dialed
// address. Wrapping the conn would break TLS handshakes.
type safeNetDialer struct {
	dialer   *net.Dialer
	security *SecurityChecker
}

// DialContext resolves host, rejects private IPs, and connects to the first safe IP.
func (d *safeNetDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("split host/port: %w", err)
	}

	resolver := &net.Resolver{PreferGo: true}
	ips, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDNSFailed, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("%w: no IP addresses resolved for %q", ErrDNSFailed, host)
	}

	var lastErr error
	for _, ip := range ips {
		if isPrivateIP(ip.IP) {
			lastErr = fmt.Errorf("IP %s is private", ip.IP.String())
			continue
		}
		target := net.JoinHostPort(ip.String(), port)
		conn, err := d.dialer.DialContext(ctx, network, target)
		if err != nil {
			lastErr = err
			continue
		}
		return conn, nil
	}
	return nil, fmt.Errorf("all IPs blocked or unreachable for %q: %w", host, lastErr)
}

// ---------------------------------------------------------------------------
// redirectPolicy — tiered redirect strategy.
// ---------------------------------------------------------------------------

// ErrCrossDomainRedirect signals a cross-domain redirect that should not be
// auto-followed. WebFetchManager.Fetch() catches it and returns RedirectInfo.
var ErrCrossDomainRedirect = errors.New("cross-domain redirect detected")

// redirectPolicy implements http.Client.CheckRedirect.
type redirectPolicy struct {
	config WebFetchConfig
}

// CheckRedirect implements the tiered redirect strategy:
//   - Same domain (including www variants) → auto-follow
//   - Cross-domain → stop and return ErrCrossDomainRedirect
func (p *redirectPolicy) CheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= p.config.MaxRedirects {
		return fmt.Errorf("stopped after %d redirects", p.config.MaxRedirects)
	}
	if len(via) == 0 {
		return nil
	}
	originalHost := strings.ToLower(via[0].URL.Hostname())
	currentHost := strings.ToLower(req.URL.Hostname())

	// Same domain (including www variants) → follow.
	if stripWWW(originalHost) == stripWWW(currentHost) {
		return nil
	}

	// Cross-domain → do not follow.
	return ErrCrossDomainRedirect
}

func stripWWW(host string) string {
	return strings.TrimPrefix(strings.ToLower(host), "www.")
}

