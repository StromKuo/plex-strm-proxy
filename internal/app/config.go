package app

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config contains the runtime configuration for the proxy.
type Config struct {
	ListenAddr       string
	PlexUpstream     *url.URL
	PlexCallbackURL  *url.URL
	STRMRoots        []string
	PlaybackMode     string
	RedirectStatus   int
	ResolverCacheTTL time.Duration
	MappingCacheTTL  time.Duration
	AllowHTTP        bool
	AllowHTTPS       bool
	AllowPrivate     bool
	AllowedPorts     []int
	MaxSTRMSize      int64
	UpstreamTimeout  time.Duration
	MaxRedirects     int
	MaxPlexBodyBytes int64
	ProxyFallback    bool
	DecisionRewrite  bool
	StrictPartMap    bool
	HLSTranscode     bool
	HLSCopyFirst     bool
	FFmpegPath       string
	TranscodeRoot    string
	TranscodeTTL     time.Duration
}

func DefaultConfig() Config {
	upstream, _ := url.Parse("http://plex:32400")
	return Config{
		ListenAddr:       "0.0.0.0:3001",
		PlexUpstream:     upstream,
		STRMRoots:        []string{"/media"},
		PlaybackMode:     "redirect",
		RedirectStatus:   302,
		ResolverCacheTTL: 5 * time.Minute,
		MappingCacheTTL:  10 * time.Minute,
		AllowHTTP:        true,
		AllowHTTPS:       true,
		AllowPrivate:     false,
		AllowedPorts:     []int{80, 443},
		MaxSTRMSize:      1 << 20,
		UpstreamTimeout:  30 * time.Second,
		MaxRedirects:     5,
		MaxPlexBodyBytes: 4 << 20,
		ProxyFallback:    false,
		DecisionRewrite:  true,
		StrictPartMap:    true,
		HLSTranscode:     true,
		HLSCopyFirst:     true,
		FFmpegPath:       "/usr/bin/ffmpeg",
		TranscodeRoot:    "/tmp/plex-strm-proxy",
		TranscodeTTL:     2 * time.Hour,
	}
}

func LoadConfig() (Config, error) {
	cfg := DefaultConfig()
	cfg.ListenAddr = envString("LISTEN_ADDR", cfg.ListenAddr)

	upstreamText := envString("PLEX_UPSTREAM", cfg.PlexUpstream.String())
	upstream, err := url.Parse(upstreamText)
	if err != nil {
		return Config{}, fmt.Errorf("PLEX_UPSTREAM: %w", err)
	}
	cfg.PlexUpstream = upstream

	if callbackText := envString("PLEX_CALLBACK_URL", ""); callbackText != "" {
		callback, err := url.Parse(callbackText)
		if err != nil {
			return Config{}, fmt.Errorf("PLEX_CALLBACK_URL: %w", err)
		}
		cfg.PlexCallbackURL = callback
	}

	if roots := envString("STRM_ROOTS", ""); roots != "" {
		cfg.STRMRoots = splitList(roots)
	} else if root := envString("STRM_ROOT", ""); root != "" {
		cfg.STRMRoots = []string{root}
	}

	cfg.PlaybackMode = strings.ToLower(envString("PLAYBACK_MODE", cfg.PlaybackMode))
	cfg.RedirectStatus, err = envInt("REDIRECT_STATUS", cfg.RedirectStatus)
	if err != nil {
		return Config{}, err
	}
	cfg.ResolverCacheTTL, err = envDuration("RESOLVER_CACHE_TTL", cfg.ResolverCacheTTL)
	if err != nil {
		return Config{}, err
	}
	cfg.MappingCacheTTL, err = envDuration("MAPPING_CACHE_TTL", cfg.MappingCacheTTL)
	if err != nil {
		return Config{}, err
	}
	cfg.AllowHTTP, err = envBool("ALLOW_HTTP", cfg.AllowHTTP)
	if err != nil {
		return Config{}, err
	}
	cfg.AllowHTTPS, err = envBool("ALLOW_HTTPS", cfg.AllowHTTPS)
	if err != nil {
		return Config{}, err
	}
	cfg.AllowPrivate, err = envBool("ALLOW_PRIVATE_TARGETS", cfg.AllowPrivate)
	if err != nil {
		return Config{}, err
	}
	cfg.AllowedPorts, err = envPorts("ALLOWED_TARGET_PORTS", cfg.AllowedPorts)
	if err != nil {
		return Config{}, err
	}
	cfg.ProxyFallback, err = envBool("PROXY_FALLBACK", cfg.ProxyFallback)
	if err != nil {
		return Config{}, err
	}
	cfg.DecisionRewrite, err = envBool("DECISION_REWRITE_PATH", cfg.DecisionRewrite)
	if err != nil {
		return Config{}, err
	}
	cfg.StrictPartMap, err = envBool("STRICT_PART_MAPPING", cfg.StrictPartMap)
	if err != nil {
		return Config{}, err
	}
	cfg.HLSTranscode, err = envBool("STRM_HLS_TRANSCODE", cfg.HLSTranscode)
	if err != nil {
		return Config{}, err
	}
	cfg.HLSCopyFirst, err = envBool("STRM_HLS_COPY_FIRST", cfg.HLSCopyFirst)
	if err != nil {
		return Config{}, err
	}
	cfg.FFmpegPath = envString("FFMPEG_PATH", cfg.FFmpegPath)
	cfg.TranscodeRoot = envString("TRANSCODE_ROOT", cfg.TranscodeRoot)
	cfg.TranscodeTTL, err = envDuration("TRANSCODE_SESSION_TTL", cfg.TranscodeTTL)
	if err != nil {
		return Config{}, err
	}
	cfg.MaxSTRMSize, err = envInt64("MAX_STRM_SIZE", cfg.MaxSTRMSize)
	if err != nil {
		return Config{}, err
	}
	cfg.UpstreamTimeout, err = envDuration("UPSTREAM_TIMEOUT_SECS", cfg.UpstreamTimeout)
	if err != nil {
		return Config{}, err
	}
	cfg.MaxRedirects, err = envInt("MAX_REDIRECTS", cfg.MaxRedirects)
	if err != nil {
		return Config{}, err
	}
	cfg.MaxPlexBodyBytes, err = envInt64("MAX_PLEX_BODY_BYTES", cfg.MaxPlexBodyBytes)
	if err != nil {
		return Config{}, err
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.ListenAddr == "" {
		return fmt.Errorf("LISTEN_ADDR must not be empty")
	}
	if c.PlexUpstream == nil || c.PlexUpstream.Host == "" {
		return fmt.Errorf("PLEX_UPSTREAM must include a host")
	}
	if c.PlexUpstream.Scheme != "http" && c.PlexUpstream.Scheme != "https" {
		return fmt.Errorf("PLEX_UPSTREAM must use http or https")
	}
	if c.PlexCallbackURL != nil {
		if c.PlexCallbackURL.Scheme != "http" && c.PlexCallbackURL.Scheme != "https" {
			return fmt.Errorf("PLEX_CALLBACK_URL must use http or https")
		}
		if c.PlexCallbackURL.Host == "" {
			return fmt.Errorf("PLEX_CALLBACK_URL must include a host")
		}
		if c.PlexCallbackURL.User != nil {
			return fmt.Errorf("PLEX_CALLBACK_URL must not include user information")
		}
		if c.PlexCallbackURL.RawQuery != "" || c.PlexCallbackURL.Fragment != "" {
			return fmt.Errorf("PLEX_CALLBACK_URL must not include a query or fragment")
		}
		if c.PlexCallbackURL.Path != "" && c.PlexCallbackURL.Path != "/" {
			return fmt.Errorf("PLEX_CALLBACK_URL must not include a path")
		}
	}
	if len(c.STRMRoots) == 0 {
		return fmt.Errorf("at least one STRM root is required")
	}
	if c.PlaybackMode != "redirect" && c.PlaybackMode != "proxy" {
		return fmt.Errorf("PLAYBACK_MODE must be redirect or proxy")
	}
	if c.RedirectStatus != 302 && c.RedirectStatus != 307 {
		return fmt.Errorf("REDIRECT_STATUS must be 302 or 307")
	}
	if c.ResolverCacheTTL <= 0 || c.MappingCacheTTL <= 0 {
		return fmt.Errorf("cache TTLs must be positive")
	}
	if c.MaxSTRMSize <= 0 || c.MaxPlexBodyBytes <= 0 {
		return fmt.Errorf("size limits must be positive")
	}
	for _, port := range c.AllowedPorts {
		if port < 1 || port > 65535 {
			return fmt.Errorf("ALLOWED_TARGET_PORTS contains invalid port %d", port)
		}
	}
	if c.UpstreamTimeout <= 0 || c.MaxRedirects < 0 {
		return fmt.Errorf("upstream timeout and redirect limit are invalid")
	}
	if c.HLSTranscode && strings.TrimSpace(c.FFmpegPath) == "" {
		return fmt.Errorf("FFMPEG_PATH must not be empty when STRM_HLS_TRANSCODE is enabled")
	}
	if c.HLSTranscode && strings.TrimSpace(c.TranscodeRoot) == "" {
		return fmt.Errorf("TRANSCODE_ROOT must not be empty when STRM_HLS_TRANSCODE is enabled")
	}
	if c.TranscodeTTL <= 0 {
		return fmt.Errorf("TRANSCODE_SESSION_TTL must be positive")
	}
	return nil
}

func envString(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return fallback
}

func splitList(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}

func envInt(key string, fallback int) (int, error) {
	text := envString(key, strconv.Itoa(fallback))
	value, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid integer %q: %w", key, text, err)
	}
	return value, nil
}

func envInt64(key string, fallback int64) (int64, error) {
	text := envString(key, strconv.FormatInt(fallback, 10))
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid integer %q: %w", key, text, err)
	}
	return value, nil
}

func envBool(key string, fallback bool) (bool, error) {
	text := envString(key, strconv.FormatBool(fallback))
	value, err := strconv.ParseBool(text)
	if err != nil {
		return false, fmt.Errorf("%s: invalid boolean %q: %w", key, text, err)
	}
	return value, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	text := envString(key, fallback.String())
	if seconds, err := strconv.ParseInt(text, 10, 64); err == nil {
		return time.Duration(seconds) * time.Second, nil
	}
	value, err := time.ParseDuration(text)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration %q: %w", key, text, err)
	}
	return value, nil
}

func envPorts(key string, fallback []int) ([]int, error) {
	defaultValue := make([]string, 0, len(fallback))
	for _, port := range fallback {
		defaultValue = append(defaultValue, strconv.Itoa(port))
	}
	text := envString(key, strings.Join(defaultValue, ","))
	if text == "" {
		return nil, nil
	}
	parts := splitList(text)
	ports := make([]int, 0, len(parts))
	for _, part := range parts {
		port, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("%s: invalid port %q: %w", key, part, err)
		}
		ports = append(ports, port)
	}
	return ports, nil
}
