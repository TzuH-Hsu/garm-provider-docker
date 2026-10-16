package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/distribution/reference"
)

// Registry settings for the DinD sidecar's daemon (ADR-001 amendment, issue
// #26): registry_mirrors and insecure_registries are emitted as the typed
// dockerd flags --registry-mirror / --insecure-registry (spec.DindCommand).
// They are operator config only: no extra_specs key, and no per-flavor
// override (a provider config describes one Docker host, and a mirror is a
// property of that host's network).
//
// The accepted forms follow dockerd's own registry configuration
// (daemon/pkg/registry/config.go in moby), deliberately a little STRICTER where
// dockerd is lenient, so that a mistake fails at load with a message instead of
// being silently normalised or surfacing later as a sidecar that cannot start:
//
//   - a mirror must be a bare origin (scheme://host[:port][/]) with a
//     lowercase scheme; dockerd also tolerates a path, which this provider does
//     not allow, and mishandles an uppercase scheme;
//   - an insecure registry must not carry a scheme; dockerd strips an http://
//     or https:// prefix with a warning;
//   - duplicates are rejected; dockerd drops them silently;
//   - ports are 1-65535; dockerd accepts 0.
//
// Entries are validated regardless of dind_mode, so a typo is caught at load
// rather than when DinD is later enabled (the same stance as dind_image).

const (
	maxPort = 65535

	// registryMirrorsKey and insecureRegistriesKey name the TOML keys in
	// errors.
	registryMirrorsKey    = "registry_mirrors"
	insecureRegistriesKey = "insecure_registries"
)

// hostPortPattern is the reference grammar for a registry domain with an
// optional port, anchored (dockerd's validHostPortRegex).
var hostPortPattern = regexp.MustCompile("^" + reference.DomainRegexp.String() + "$")

// validateRegistryMirrors checks every registry_mirrors entry and rejects
// duplicates (compared case-insensitively, ignoring one trailing slash).
func (c Config) validateRegistryMirrors() error {
	seen := make(map[string]struct{}, len(c.RegistryMirrors))
	for i, raw := range c.RegistryMirrors {
		if err := validateRegistryMirror(raw); err != nil {
			return fmt.Errorf("%s[%d]: %w", registryMirrorsKey, i, err)
		}
		key := strings.ToLower(strings.TrimSuffix(raw, "/"))
		if _, dup := seen[key]; dup {
			return fmt.Errorf("%s[%d] %q is a duplicate of an earlier entry", registryMirrorsKey, i, raw)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// validateInsecureRegistries checks every insecure_registries entry and
// rejects duplicates (hosts case-insensitively, CIDRs by their masked form).
func (c Config) validateInsecureRegistries() error {
	seen := make(map[string]struct{}, len(c.InsecureRegistries))
	for i, raw := range c.InsecureRegistries {
		key, err := validateInsecureRegistry(raw)
		if err != nil {
			return fmt.Errorf("%s[%d] %q: %w", insecureRegistriesKey, i, raw, err)
		}
		if _, dup := seen[key]; dup {
			return fmt.Errorf("%s[%d] %q is a duplicate of an earlier entry", insecureRegistriesKey, i, raw)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// validateRegistryMirror accepts scheme://host[:port][/] with an http or https
// scheme and nothing else. The offending value is echoed back in the error
// except where it could carry credentials.
func validateRegistryMirror(raw string) error {
	if strings.TrimSpace(raw) != raw || raw == "" {
		return fmt.Errorf("%q must not be empty or have leading or trailing whitespace", raw)
	}
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok || scheme == "" {
		return fmt.Errorf("%q must be an absolute URL with an http:// or https:// scheme", raw)
	}
	// Lowercase only: dockerd's classic pull path checks the configured string
	// for a literal "http://" / "https://" prefix and would otherwise prepend
	// "https://" to an "HTTPS://..." mirror.
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("%q has unsupported scheme %q: must be lowercase http or https", raw, scheme)
	}

	authority := rest
	if end := strings.IndexAny(rest, "/?#"); end >= 0 {
		authority = rest[:end]
	}
	if strings.Contains(authority, "@") {
		// Never echo the value: it holds credentials.
		return errors.New("userinfo (user:password@) is not allowed in a mirror URL")
	}
	if strings.Contains(rest, "?") {
		return fmt.Errorf("%q must not have a query", raw)
	}
	if strings.Contains(rest, "#") {
		return fmt.Errorf("%q must not have a fragment", raw)
	}

	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%q is not a valid URL: %w", raw, err)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("%q has no host", raw)
	}
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("%q must not have a path (only a bare origin is accepted)", raw)
	}
	if err := validateHostPort(u.Host); err != nil {
		return fmt.Errorf("%q: %w", raw, err)
	}
	return nil
}

// validateInsecureRegistry accepts host[:port] or a CIDR, as dockerd does, and
// returns the normalised duplicate-detection key.
func validateInsecureRegistry(raw string) (string, error) {
	if strings.TrimSpace(raw) != raw || raw == "" {
		return "", errors.New("must not be empty or have leading or trailing whitespace")
	}
	if strings.Contains(raw, "://") {
		return "", errors.New("must not include a scheme (use host[:port] or a CIDR, not a URL)")
	}
	if strings.HasPrefix(raw, "-") || strings.HasSuffix(raw, "-") {
		return "", errors.New("invalid index name: cannot begin or end with a hyphen")
	}
	if prefix, err := netip.ParsePrefix(raw); err == nil {
		return prefix.Masked().String(), nil
	}
	if strings.Contains(raw, "/") {
		return "", errors.New("is not a valid CIDR, and a registry host[:port] cannot contain '/'")
	}
	if err := validateHostPort(raw); err != nil {
		return "", err
	}
	return strings.ToLower(raw), nil
}

// validateHostPort mirrors moby's validateHostPort: s is a registry domain
// (optionally with a numeric port) or an IP address, bracketed when it
// carries a port.
func validateHostPort(s string) error {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		// No port, or a bare IPv6 address.
		host, port = s, ""
	}
	if !hostPortPattern.MatchString(s) && net.ParseIP(host) == nil {
		return fmt.Errorf("invalid host %q", host)
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > maxPort {
			return fmt.Errorf("invalid port %q: must be 1-%d", port, maxPort)
		}
	}
	return nil
}
