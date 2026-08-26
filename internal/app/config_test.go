package app

import (
	"net/url"
	"testing"
)

func TestLoadConfigAcceptsPlexCallbackURL(t *testing.T) {
	t.Setenv("PLEX_CALLBACK_URL", "http://plex-strm-proxy:3001")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PlexCallbackURL == nil || cfg.PlexCallbackURL.String() != "http://plex-strm-proxy:3001" {
		t.Fatalf("unexpected Plex callback URL: %#v", cfg.PlexCallbackURL)
	}
}

func TestConfigRejectsPlexCallbackURLWithCredentialsOrQuery(t *testing.T) {
	tests := []string{
		"http://user:pass@plex-strm-proxy:3001",
		"http://plex-strm-proxy:3001/callback",
		"http://plex-strm-proxy:3001?token=secret",
	}
	for _, value := range tests {
		t.Run(value, func(t *testing.T) {
			cfg := DefaultConfig()
			callback, err := url.Parse(value)
			if err != nil {
				t.Fatal(err)
			}
			cfg.PlexCallbackURL = callback
			if err := cfg.Validate(); err == nil {
				t.Fatalf("Validate() accepted unsafe callback URL %q", value)
			}
		})
	}
}
