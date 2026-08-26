package app

import (
	"net/http"
	"strings"
)

// proxyTranscodeFormat is the wire format expected by the playback client.
// Playback policy decides whether the proxy owns the fallback; the request
// format decides whether that fallback is HLS or DASH.
type proxyTranscodeFormat string

const (
	proxyTranscodeHLS  proxyTranscodeFormat = "hls"
	proxyTranscodeDASH proxyTranscodeFormat = "dash"
)

func transcodeFormatForRequest(request *http.Request) proxyTranscodeFormat {
	if request == nil || request.URL == nil {
		return proxyTranscodeHLS
	}
	query := request.URL.Query()
	if isHLSPlayback(query) || isHLSStartRequest(request) {
		return proxyTranscodeHLS
	}
	if query.Get("protocol") == "dash" || hasSuffixIgnoreCase(request.URL.Path, ".mpd") {
		return proxyTranscodeDASH
	}
	// Existing clients that omit protocol have historically received HLS.
	// Keep that behavior while making explicit DASH requests strict.
	return proxyTranscodeHLS
}

func hasSuffixIgnoreCase(value, suffix string) bool {
	if len(value) < len(suffix) {
		return false
	}
	return strings.EqualFold(value[len(value)-len(suffix):], suffix)
}
