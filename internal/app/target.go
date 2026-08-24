package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

type TargetPolicy struct {
	AllowHTTP     bool
	AllowHTTPS    bool
	AllowPrivate  bool
	AllowedPorts  []int
	MaxRedirects  int
	HeaderTimeout time.Duration
}

func (p TargetPolicy) ValidateRawURL(ctx context.Context, rawURL string) (*url.URL, error) {
	target, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("parse media URL: %w", err)
	}
	if err := p.ValidateURL(ctx, target); err != nil {
		return nil, err
	}
	return target, nil
}

func (p TargetPolicy) ValidateURL(ctx context.Context, target *url.URL) error {
	if target == nil || target.Hostname() == "" {
		return fmt.Errorf("media URL has no host")
	}
	switch strings.ToLower(target.Scheme) {
	case "http":
		if !p.AllowHTTP {
			return fmt.Errorf("http media URLs are disabled")
		}
	case "https":
		if !p.AllowHTTPS {
			return fmt.Errorf("https media URLs are disabled")
		}
	default:
		return ErrUnsupportedScheme
	}
	port := target.Port()
	if port == "" {
		if strings.EqualFold(target.Scheme, "https") {
			port = "443"
		} else {
			port = "80"
		}
	}
	if len(p.AllowedPorts) > 0 && !containsPort(p.AllowedPorts, port) {
		return fmt.Errorf("media target port %s is not allowed", port)
	}
	if p.AllowPrivate {
		return nil
	}
	host := strings.TrimSuffix(strings.ToLower(target.Hostname()), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return fmt.Errorf("private media targets are disabled")
	}
	if address, err := netip.ParseAddr(host); err == nil {
		if blockedAddress(address) {
			return fmt.Errorf("private media targets are disabled")
		}
		return nil
	}

	if ctx == nil {
		ctx = context.Background()
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("resolve media target: %w", err)
	}
	if len(addresses) == 0 {
		return fmt.Errorf("media target has no address")
	}
	for _, address := range addresses {
		parsed, parseErr := netip.ParseAddr(address.IP.String())
		if parseErr == nil && blockedAddress(parsed) {
			return fmt.Errorf("private media targets are disabled")
		}
	}
	return nil
}

func containsPort(allowed []int, value string) bool {
	for _, port := range allowed {
		if fmt.Sprintf("%d", port) == value {
			return true
		}
	}
	return false
}

func blockedAddress(address netip.Addr) bool {
	address = address.Unmap()
	return address.IsLoopback() || address.IsPrivate() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsUnspecified() || address.IsMulticast()
}

func NewMediaClient(policy TargetPolicy) *http.Client {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: policy.HeaderTimeout,
		DialContext:           safeDialContext(policy),
	}

	return &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= policy.MaxRedirects {
				return http.ErrUseLastResponse
			}
			return policy.ValidateURL(req.Context(), req.URL)
		},
	}
}

// ResolveMediaTarget follows redirects only through the policy-bound media
// client and returns the final validated URL. Subprocess callers then disable
// their own redirect handling, so a redirect cannot escape TargetPolicy.
func ResolveMediaTarget(ctx context.Context, client *http.Client, policy TargetPolicy, rawURL string) (string, error) {
	if client == nil {
		return "", fmt.Errorf("media client is required")
	}
	target, err := policy.ValidateRawURL(ctx, rawURL)
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return "", fmt.Errorf("create media validation request: %w", err)
	}
	request.Header.Set("Range", "bytes=0-0")
	request.Header.Set("User-Agent", "plex-strm-proxy/1.0")
	response, err := client.Do(request)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) && urlErr.Err != nil {
			return "", fmt.Errorf("validate media target redirects: %w", urlErr.Err)
		}
		return "", fmt.Errorf("validate media target redirects: request failed")
	}
	defer response.Body.Close()
	if response.StatusCode >= http.StatusMultipleChoices && response.StatusCode < http.StatusBadRequest {
		return "", fmt.Errorf("media target has an unresolved redirect (HTTP %d)", response.StatusCode)
	}
	if response.Request == nil || response.Request.URL == nil {
		return "", fmt.Errorf("media target validation returned no final URL")
	}
	if err := policy.ValidateURL(ctx, response.Request.URL); err != nil {
		return "", fmt.Errorf("final media target is not allowed: %w", err)
	}
	return response.Request.URL.String(), nil
}

func safeDialContext(policy TargetPolicy) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if policy.AllowPrivate {
		return dialer.DialContext
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		var lastErr error
		for _, ip := range ips {
			parsed, parseErr := netip.ParseAddr(ip.IP.String())
			if parseErr != nil || blockedAddress(parsed) {
				continue
			}
			conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
			if dialErr == nil {
				return conn, nil
			}
			lastErr = dialErr
		}
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, fmt.Errorf("no permitted address for media target %q", host)
	}
}
