// Package endpoint validates public peer endpoints. Local agent endpoints have
// a different contract and must not use this package.
package endpoint

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

var ErrInvalid = errors.New("invalid remote peer endpoint")

// Parse returns a canonical https peer URL and rejects URL features that make
// certificate identity, logging, or routing ambiguous.
func Parse(raw string) (url.URL, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return url.URL{}, fmt.Errorf("%w: endpoint is empty", ErrInvalid)
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return url.URL{}, fmt.Errorf("%w: endpoint must use https:// with a host", ErrInvalid)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return url.URL{}, fmt.Errorf("%w: endpoint cannot contain credentials, query, or fragment", ErrInvalid)
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return url.URL{}, fmt.Errorf("%w: endpoint cannot contain a path", ErrInvalid)
	}
	if parsed.Hostname() == "" {
		return url.URL{}, fmt.Errorf("%w: hostname is required", ErrInvalid)
	}
	if strings.Contains(parsed.Hostname(), " ") {
		return url.URL{}, fmt.Errorf("%w: hostname contains whitespace", ErrInvalid)
	}
	if err := validateExplicitPort(parsed.Host); err != nil {
		return url.URL{}, err
	}
	parsed.Path = ""
	parsed.RawPath = ""
	return *parsed, nil
}

// validateExplicitPort rejects authorities whose explicit port is not a
// number in 1-65535. url.Parse keeps out-of-range and empty ports verbatim
// and its Port accessor reports an empty string for anything non-numeric —
// and deliberately discarded the resolution error — so the raw authority is
// inspected: a mistyped port must fail here, at registration, rather than at
// the first transfer attempt.
func validateExplicitPort(host string) error {
	if strings.HasPrefix(host, "[") {
		end := strings.Index(host, "]")
		if end < 0 {
			return fmt.Errorf("%w: IPv6 literal is not closed", ErrInvalid)
		}
		rest := host[end+1:]
		if rest == "" {
			return nil
		}
		if !strings.HasPrefix(rest, ":") {
			return fmt.Errorf("%w: port is invalid", ErrInvalid)
		}
		return validatePortNumber(rest[1:])
	}
	switch strings.Count(host, ":") {
	case 0:
		return nil
	case 1:
		return validatePortNumber(host[strings.Index(host, ":")+1:])
	default:
		return fmt.Errorf("%w: IPv6 literals must be written in brackets", ErrInvalid)
	}
}

func validatePortNumber(port string) error {
	if port == "" {
		return fmt.Errorf("%w: port is empty", ErrInvalid)
	}
	for _, r := range port {
		if r < '0' || r > '9' {
			return fmt.Errorf("%w: port is invalid", ErrInvalid)
		}
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return fmt.Errorf("%w: port %q is outside 1-65535", ErrInvalid, port)
	}
	return nil
}

func Normalize(raw string) (string, error) {
	parsed, err := Parse(raw)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func ServerName(raw, override string) (string, error) {
	if strings.TrimSpace(override) != "" {
		name := strings.TrimSpace(override)
		if strings.ContainsAny(name, "/?#@ \t\r\n") {
			return "", fmt.Errorf("%w: server name is invalid", ErrInvalid)
		}
		return name, nil
	}
	parsed, err := Parse(raw)
	if err != nil {
		return "", err
	}
	return parsed.Hostname(), nil
}
