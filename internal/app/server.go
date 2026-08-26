package app

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

type contextKey string

const (
	requestIDKey      contextKey = "request-id"
	defaultSTRMKey    contextKey = "default-strm-path"
	nativeSTRMKey     contextKey = "native-strm-plex"
	plexProxyErrorKey contextKey = "plex-proxy-error"
)

type Server struct {
	cfg             Config
	resolver        *Resolver
	mappings        *MappingStore
	policy          TargetPolicy
	mediaClient     *http.Client
	plexClient      *http.Client
	plexProxy       *httputil.ReverseProxy
	hls             *hlsTranscoder
	logger          *slog.Logger
	httpServer      *http.Server
	decisionMu      sync.Mutex
	directDecisions map[string]directDecision
	metadataMu      sync.Mutex
	metadataLookups map[string]metadataPartLookupCacheEntry
	probeMu         sync.Mutex
	probes          map[string]mediaProbeCacheEntry
	probeFlights    map[string]*mediaProbeFlight
}

type directDecision struct {
	mapping      PartMapping
	metadataPath string
	contentType  string
	body         []byte
	createdAt    time.Time
	expiresAt    time.Time
}

// plexProxyErrorCapture lets the buffered native Plex path distinguish an
// actual upstream rejection from a request that the client cancelled while
// Plex was still opening the transcode session. The latter must not create a
// proxy-owned HLS session: doing so races the client's retry and takes
// ownership away from Plex.
type plexProxyErrorCapture struct {
	mu  sync.Mutex
	err error
}

func (c *plexProxyErrorCapture) set(err error) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.err = err
	c.mu.Unlock()
}

func (c *plexProxyErrorCapture) get() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// mediaProbeFlight coalesces the first direct-play probe with the follow-up
// decision request. The first decision is allowed to return immediately while
// the second decision waits for the same probe instead of starting a duplicate
// probe or deferring it to start.mpd.
type mediaProbeFlight struct {
	done  chan struct{}
	value mediaProbe
	ok    bool
}

func NewServer(cfg Config, logger *slog.Logger) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	resolver, err := NewResolver(cfg.STRMRoots, cfg.MaxSTRMSize, cfg.ResolverCacheTTL)
	if err != nil {
		return nil, err
	}
	mappings, err := NewMappingStore(cfg.MappingCacheTTL)
	if err != nil {
		return nil, err
	}
	policy := TargetPolicy{
		AllowHTTP:     cfg.AllowHTTP,
		AllowHTTPS:    cfg.AllowHTTPS,
		AllowPrivate:  cfg.AllowPrivate,
		AllowedPorts:  cfg.AllowedPorts,
		MaxRedirects:  cfg.MaxRedirects,
		HeaderTimeout: cfg.UpstreamTimeout,
	}

	plexTransport := newPlexTransport(cfg.UpstreamTimeout)
	server := &Server{
		cfg:             cfg,
		resolver:        resolver,
		mappings:        mappings,
		policy:          policy,
		mediaClient:     NewMediaClient(policy),
		plexClient:      &http.Client{Transport: plexTransport, CheckRedirect: noRedirects},
		logger:          logger,
		directDecisions: make(map[string]directDecision),
		metadataLookups: make(map[string]metadataPartLookupCacheEntry),
		probes:          make(map[string]mediaProbeCacheEntry),
		probeFlights:    make(map[string]*mediaProbeFlight),
	}
	if cfg.HLSTranscode {
		server.hls, err = newHLSTranscoder(cfg.TranscodeRoot, cfg.FFmpegPath, cfg.TranscodeTTL, cfg.HLSCopyFirst, policy, server.mediaClient, logger)
		if err != nil {
			return nil, err
		}
	}
	server.plexProxy = &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(cfg.PlexUpstream)
			// Preserve the public host so Plex does not generate redirects to
			// the proxy's private upstream address.
			request.Out.Host = request.In.Host
			request.SetXForwarded()
		},
		Transport:      plexTransport,
		FlushInterval:  100 * time.Millisecond,
		ModifyResponse: server.modifyPlexResponse,
		ErrorHandler: func(writer http.ResponseWriter, request *http.Request, err error) {
			if capture, ok := request.Context().Value(plexProxyErrorKey).(*plexProxyErrorCapture); ok {
				capture.set(err)
			}
			server.logger.Error("plex upstream request failed", "request_id", requestID(request.Context()), "method", request.Method, "path", request.URL.Path, "error", err)
			writeJSONError(writer, http.StatusBadGateway, "plex upstream unavailable")
		},
	}
	server.httpServer = &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return server, nil
}

func (s *Server) ListenAndServe() error {
	return s.httpServer.ListenAndServe()
}

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.serveHTTP)
}

func (s *Server) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	requestID := request.Header.Get("X-Request-ID")
	if requestID == "" {
		requestID = newRequestID()
	}
	request = request.WithContext(context.WithValue(request.Context(), requestIDKey, requestID))
	writer.Header().Set("X-Request-ID", requestID)
	tracked := newLoggingResponseWriter(writer)
	started := time.Now()

	switch {
	case isDecisionRequest(request):
		s.handleDecision(tracked, request)
	case isTranscodeStartRequest(request):
		s.handleTranscodeStart(tracked, request)
	case isHLSResourceRequest(request.URL.Path):
		s.handleTranscodeResource(tracked, request)
	case isPartFileRequest(request.URL.Path):
		s.handlePartFile(tracked, request)
	default:
		s.proxyToPlex(tracked, request, "")
	}

	s.logger.Info("request completed", "request_id", requestID, "method", request.Method, "path", request.URL.Path, "status", tracked.status, "bytes", tracked.bytes, "duration_ms", time.Since(started).Milliseconds())
}

func (s *Server) handleDecision(writer http.ResponseWriter, request *http.Request) {
	profile := parseClientPlaybackProfile(request)
	var profileVideoCodecs, profileAudioCodecs, profileProtocols, profileContainers []string
	profileHasVideo, profileHasAudio := false, false
	if profile != nil {
		profileVideoCodecs = playbackProfileValues(profile.VideoCodecs)
		profileAudioCodecs = playbackProfileValues(profile.AudioCodecs)
		profileProtocols = playbackProfileValues(profile.Protocols)
		profileContainers = playbackProfileValues(profile.Containers)
		profileHasVideo = profile.HasVideo
		profileHasAudio = profile.HasAudio
	}
	query := request.URL.Query()
	s.logger.Info("Plex decision client", "request_id", requestID(request.Context()), "product", request.Header.Get("X-Plex-Product"), "platform", request.Header.Get("X-Plex-Platform"), "model", request.Header.Get("X-Plex-Model"), "device", request.Header.Get("X-Plex-Device"), "user_agent", request.UserAgent(), "accept", request.Header.Get("Accept"), "path_kind", playbackPathKind(query.Get("path")), "protocol", query.Get("protocol"), "direct_play", query.Get("directPlay"), "direct_stream", query.Get("directStream"), "location", query.Get("location"), "container", query.Get("container"), "video_codec", query.Get("videoCodec"), "audio_codec", query.Get("audioCodec"), "video_resolution", query.Get("videoResolution"), "video_quality", query.Get("videoQuality"), "max_video_bitrate", query.Get("maxVideoBitrate"), "media_index", query.Get("mediaIndex"), "part_index", query.Get("partIndex"), "audio_stream_id", query.Get("audioStreamID"), "video_stream_id", query.Get("videoStreamID"), "profile_present", profile != nil, "profile_has_video", profileHasVideo, "profile_has_audio", profileHasAudio, "profile_video_codecs", profileVideoCodecs, "profile_audio_codecs", profileAudioCodecs, "profile_protocols", profileProtocols, "profile_containers", profileContainers, "session", hlsSessionIDFromRequest(request))
	requestedPath := request.URL.Query().Get("path")
	if metadataPath, ok := parseMetadataPath(requestedPath); ok && s.cfg.DecisionRewrite {
		lookup, err := s.lookupMetadataPartSnapshot(request.Context(), request, metadataPath)
		if err != nil {
			s.logger.Warn("failed to inspect Plex metadata for decision", "request_id", requestID(request.Context()), "metadata_path", metadataPath, "error", err)
			s.proxyToPlex(writer, request, "")
			return
		}
		mapping := lookup.Mapping
		if mapping.Kind == PartKindSTRM {
			if mapping.ResolvedURL == "" {
				s.logger.Error("STRM metadata has no resolved URL", "request_id", requestID(request.Context()), "metadata_path", metadataPath, "part_id", mapping.PartID, "resolution_error", mapping.ResolutionErr)
				writeJSONError(writer, http.StatusBadGateway, "STRM URL could not be resolved")
				return
			}
			// Let Plex evaluate the same directPlay/directStream/profile query it
			// received from the client. The only substitution is the media path:
			// Plex fetches a proxy-local metadata document whose Part points to the
			// protected local Part adapter. This gives Plex real stream metadata
			// without exposing the third-party URL or Plex credentials to it.
			query := request.URL.Query()
			query.Set("path", s.localMetadataProxyURL(metadataPath, mapping.PartID))
			decisionURL := *request.URL
			decisionURL.RawQuery = query.Encode()
			decisionRequest := request.Clone(context.WithValue(request.Context(), nativeSTRMKey, true))
			decisionRequest.URL = &decisionURL
			s.logger.Info("requesting native Plex STRM decision", "request_id", requestID(request.Context()), "metadata_path", metadataPath, "part_id", mapping.PartID, "target_host", resolvedTargetHost(mapping.ResolvedURL), "direct_play", query.Get("directPlay"), "direct_stream", query.Get("directStream"), "protocol", query.Get("protocol"))
			status, headers, body, proxyErr := s.proxyToPlexBuffered(decisionRequest, mapping.STRMPath)
			if request.Context().Err() == nil && shouldUseSTRMProxyFallback(status, proxyErr) && isSTRMTranscodeIntent(request) && s.hls != nil {
				if s.serveSTRMDecisionFallback(writer, request, mapping, lookup) {
					return
				}
			}
			writeBufferedPlexResponse(writer, status, headers, body)
			return
		}
		s.proxyToPlex(writer, request, "")
		return
	}
	if !isSTRMPath(requestedPath) {
		s.proxyToPlex(writer, request, "")
		return
	}

	resolved, err := s.resolver.Resolve(request.Context(), requestedPath)
	if err != nil {
		s.logger.Warn("failed to resolve STRM during decision", "request_id", requestID(request.Context()), "path", requestedPath, "error", err)
		status := http.StatusUnprocessableEntity
		if errors.Is(err, ErrSTRMNotFound) {
			status = http.StatusNotFound
		} else if errors.Is(err, ErrPathOutsideRoot) {
			status = http.StatusForbidden
		}
		writeJSONError(writer, status, "unable to resolve STRM file")
		return
	}

	decisionRequest := request
	if s.cfg.DecisionRewrite {
		query := prepareSTRMDirectPlaybackQuery(request, resolved.URL)
		clonedURL := *request.URL
		clonedURL.RawQuery = query.Encode()
		decisionRequest = request.Clone(request.Context())
		decisionRequest.URL = &clonedURL
		s.logger.Info("rewriting Plex decision path", "request_id", requestID(request.Context()), "strm_path", requestedPath, "target_host", resolvedTargetHost(resolved.URL))
	}
	s.proxyToPlex(writer, decisionRequest, requestedPath)
}

func isSTRMTranscodeIntent(request *http.Request) bool {
	if request == nil || request.URL == nil {
		return false
	}
	query := request.URL.Query()
	return query.Get("directPlay") == "0" && query.Get("directStream") == "0"
}

func shouldUseSTRMProxyFallback(status int, proxyErr error) bool {
	if status < http.StatusBadRequest {
		return false
	}
	return !errors.Is(proxyErr, context.Canceled) && !errors.Is(proxyErr, context.DeadlineExceeded)
}

func (s *Server) serveSTRMDecisionFallback(writer http.ResponseWriter, request *http.Request, mapping PartMapping, lookup metadataPartLookup) bool {
	if s.hls == nil {
		return false
	}
	plan, probe := s.selectPlaybackPlan(request, mapping, PlaybackStageDecisionRequest, PlexDecisionTranscode, false)
	sessionID, err := s.registerProxyTranscode(request, mapping, true, transcodeFormatForRequest(request))
	if err != nil {
		s.logger.Warn("native Plex STRM decision failed and proxy fallback could not start", "request_id", requestID(request.Context()), "part_id", mapping.PartID, "plan", plan.Plan, "reason", plan.Reason, "error", err)
		return false
	}
	s.logger.Info("native Plex STRM decision rejected; using compatibility fallback", "request_id", requestID(request.Context()), "part_id", mapping.PartID, "session", sessionID, "plan", plan.Plan, "reason", plan.Reason)
	writeProxyTranscodeDecision(writer, request, mapping, lookup.ContentType, lookup.Body, sessionID, transcodeFormatForRequest(request), proxyStreamPlanForMode(parseClientPlaybackProfile(request), probe, true))
	return true
}

func playbackPathKind(value string) string {
	value = strings.TrimSpace(value)
	if _, ok := parseMetadataPath(value); ok {
		return "metadata"
	}
	if isSTRMPath(value) {
		return "strm"
	}
	if strings.HasPrefix(strings.ToLower(value), "http://") || strings.HasPrefix(strings.ToLower(value), "https://") {
		return "url"
	}
	if value == "" {
		return "empty"
	}
	return "other"
}

func (s *Server) handleTranscodeStart(writer http.ResponseWriter, request *http.Request) {
	metadataPath, ok := parseMetadataPath(request.URL.Query().Get("path"))
	if !ok || !s.cfg.DecisionRewrite {
		// Plex Web can send the direct source URL back in start.mpd after the
		// direct-first decision, even though this is now a transcode request.
		// Rebind that fallback to the protected Part endpoint so Plex does not
		// receive the third-party URL or append its token to it.
		if request.URL.Query().Get("plexTranscode") == "1" && s.hls != nil {
			if decision, found := s.directDecisionRecordForStart(hlsSessionIDFromRequest(request), ""); found && decision.mapping.Kind == PartKindSTRM && decision.metadataPath != "" {
				query := request.URL.Query()
				query.Set("path", s.localMetadataProxyURL(decision.metadataPath, decision.mapping.PartID))
				clonedURL := *request.URL
				clonedURL.RawQuery = query.Encode()
				startRequest := request.Clone(context.WithValue(request.Context(), nativeSTRMKey, true))
				startRequest.URL = &clonedURL
				s.logger.Info("rebinding STRM transcode start through local metadata", "request_id", requestID(request.Context()), "metadata_path", decision.metadataPath, "part_id", decision.mapping.PartID, "session", hlsSessionIDFromRequest(request))
				s.proxyToPlex(writer, startRequest, decision.mapping.STRMPath)
				return
			}
		}
		s.proxyToPlex(writer, request, "")
		return
	}

	mapping, err := s.lookupMetadataPart(request.Context(), request, metadataPath)
	if err != nil {
		s.logger.Warn("failed to inspect Plex metadata for transcode start", "request_id", requestID(request.Context()), "metadata_path", metadataPath, "error", err)
		s.proxyToPlex(writer, request, "")
		return
	}
	if mapping.Kind != PartKindSTRM {
		s.proxyToPlex(writer, request, "")
		return
	}
	if mapping.ResolvedURL == "" {
		s.logger.Error("STRM metadata has no resolved URL for transcode start", "request_id", requestID(request.Context()), "metadata_path", metadataPath, "part_id", mapping.PartID, "resolution_error", mapping.ResolutionErr)
		writeJSONError(writer, http.StatusBadGateway, "STRM URL could not be resolved")
		return
	}
	sessionID := hlsSessionIDFromRequest(request)
	s.logger.Info("STRM transcode start request", "request_id", requestID(request.Context()), "metadata_path", metadataPath, "part_id", mapping.PartID, "protocol", request.URL.Query().Get("protocol"), "direct_play", request.URL.Query().Get("directPlay"), "direct_stream", request.URL.Query().Get("directStream"), "session", sessionID, "location", request.URL.Query().Get("location"))

	// A proxy-owned session is retained for compatibility with an already
	// established fallback session. New STRM playback never creates one here:
	// the Plex server must receive the original start request and own the
	// stream-level transcode decision.
	if s.hls != nil {
		if format, ok := s.hls.sessionFormat(sessionID); ok {
			s.logger.Info("starting existing STRM proxy transcode session", "request_id", requestID(request.Context()), "metadata_path", metadataPath, "part_id", mapping.PartID, "session", sessionID, "format", format)
			s.handleProxyTranscodeStart(writer, request, sessionID)
			return
		}
	}
	// A start request with both flags set to zero is an explicit request for a
	// server-owned transcode. A prior direct decision in the same session must
	// not override that current intent.
	if !isSTRMTranscodeIntent(request) {
		if directMapping, ok := s.directDecisionForStart(sessionID, mapping.PartID); ok {
			s.logger.Info("redirecting confirmed STRM direct start to direct source", "request_id", requestID(request.Context()), "part_id", directMapping.PartID, "target_host", resolvedTargetHost(directMapping.ResolvedURL))
			s.serveResolvedMedia(writer, request, directMapping)
			return
		}
	}

	// Preserve every client capability and intent parameter. Plex receives a
	// local metadata URL instead of the .strm path; that document contains the
	// probed video/audio streams and a protected local Part URL, so Plex can run
	// its normal start/manifest flow without seeing the third-party source.
	query := request.URL.Query()
	query.Set("path", s.localMetadataProxyURL(metadataPath, mapping.PartID))
	s.logger.Info("forwarding native STRM transcode start to Plex", "request_id", requestID(request.Context()), "metadata_path", metadataPath, "part_id", mapping.PartID, "target_host", resolvedTargetHost(mapping.ResolvedURL), "protocol", query.Get("protocol"), "direct_play", query.Get("directPlay"), "direct_stream", query.Get("directStream"))
	clonedURL := *request.URL
	clonedURL.RawQuery = query.Encode()
	startRequest := request.Clone(context.WithValue(request.Context(), nativeSTRMKey, true))
	startRequest.URL = &clonedURL
	status, headers, body, proxyErr := s.proxyToPlexBuffered(startRequest, mapping.STRMPath)
	if request.Context().Err() == nil && shouldUseSTRMProxyFallback(status, proxyErr) && s.hls != nil {
		format := transcodeFormatForRequest(request)
		if fallbackID, registerErr := s.registerProxyTranscode(request, mapping, isSTRMTranscodeIntent(request), format); registerErr == nil {
			s.logger.Info("native Plex STRM transcode start rejected; using compatibility fallback", "request_id", requestID(request.Context()), "metadata_path", metadataPath, "part_id", mapping.PartID, "session", fallbackID, "format", format)
			s.handleProxyTranscodeStart(writer, request, fallbackID)
			return
		} else {
			s.logger.Warn("native Plex STRM transcode start failed and proxy fallback could not start", "request_id", requestID(request.Context()), "metadata_path", metadataPath, "part_id", mapping.PartID, "error", registerErr)
		}
	}
	writeBufferedPlexResponse(writer, status, headers, body)
}

func (s *Server) metadataProxyURL(request *http.Request, metadataPath, partID string) string {
	query := url.Values{"plexTranscode": {"1"}}
	if strings.TrimSpace(partID) != "" {
		query.Set("plexPartID", partID)
	}
	return s.publicProxyURL(request, metadataPath, query.Encode())
}

func (s *Server) localMetadataProxyURL(metadataPath, partID string) string {
	query := url.Values{"plexTranscode": {"1"}}
	if strings.TrimSpace(partID) != "" {
		query.Set("plexPartID", partID)
	}
	return s.plexCallbackProxyURL(metadataPath, query.Encode())
}

func (s *Server) partProxyURL(request *http.Request, partID string) string {
	return s.publicProxyURL(request, "/library/parts/"+url.PathEscape(partID)+"/file", "plexTranscode=1")
}

func (s *Server) publicProxyURL(request *http.Request, endpointPath, rawQuery string) string {
	scheme := strings.TrimSpace(request.Header.Get("X-Forwarded-Proto"))
	if scheme == "" {
		scheme = "http"
	}
	host := strings.TrimSpace(request.Host)
	if host == "" {
		host = s.cfg.ListenAddr
	}
	return (&url.URL{
		Scheme:   scheme,
		Host:     host,
		Path:     endpointPath,
		RawQuery: rawQuery,
	}).String()
}

func (s *Server) localProxyURL(endpointPath, rawQuery string) string {
	host, port := s.localProxyHostPort()
	return (&url.URL{
		Scheme:   "http",
		Host:     net.JoinHostPort(host, port),
		Path:     endpointPath,
		RawQuery: rawQuery,
	}).String()
}

// plexCallbackProxyURL builds the proxy URL that Plex must fetch while making
// its native STRM decision or opening a native transcode. It is deliberately
// separate from localProxyURL: Plex may be in another container/network
// namespace, while ffprobe/ffmpeg still need an address reachable from this
// process itself.
func (s *Server) plexCallbackProxyURL(endpointPath, rawQuery string) string {
	if callback := s.cfg.PlexCallbackURL; callback != nil {
		callbackURL := *callback
		callbackURL.Path = endpointPath
		callbackURL.RawPath = ""
		callbackURL.RawQuery = rawQuery
		callbackURL.Fragment = ""
		return callbackURL.String()
	}
	return s.localProxyURL(endpointPath, rawQuery)
}

// registerProxyTranscode registers a proxy-owned playback session for an STRM
// part. forceTranscode must be set when the client already stated it cannot
// decode the source, so the session skips the copy-first attempt.
func (s *Server) registerProxyTranscode(request *http.Request, mapping PartMapping, forceTranscode bool, format proxyTranscodeFormat) (string, error) {
	if s.hls == nil {
		return "", fmt.Errorf("proxy transcoder is disabled")
	}
	probeRequest := request
	if request != nil {
		// Plex Web may cancel the decision/start request after receiving its
		// synthetic response. Media probing is part of starting the proxy
		// session, so it must not be discarded with that upstream request.
		probeRequest = request.Clone(context.WithoutCancel(request.Context()))
	}
	plan, probe := s.selectPlaybackPlan(probeRequest, mapping, PlaybackStageTranscodeStart, PlexDecisionTranscode, false)
	profile := parseClientPlaybackProfile(probeRequest)
	// If the source probe is unavailable, copy-first still applies to video,
	// but unknown audio must not be advertised or muxed as a direct copy. AAC
	// is the smallest safe compatibility fallback; Plex remains the primary
	// decision owner and this branch is reached only after its native flow
	// failed.
	audioTranscode := probe == nil || plan.Plan == PlaybackPlanProxyHLSAudioFallback || sourceAudioNeedsTranscode(profile, probe)
	audioStreamIndex := -1
	if probe != nil {
		audioStreamIndex = preferredAudioIndex(*probe)
	}
	if !forceTranscode && sourceVideoNeedsTranscode(profile, probe) {
		forceTranscode = true
		s.logger.Info("forcing STRM video transcode for client codec compatibility", "request_id", requestID(request.Context()), "part_id", mapping.PartID, "video_codec", probe.VideoCodec, "format", format)
	}
	videoCodecString := ""
	if probe != nil && !forceTranscode && strings.EqualFold(probe.VideoCodec, "hevc") {
		videoCodecString = probe.VideoCodecString
	}
	// FFmpeg reads through the proxy's protected Part adapter. The adapter uses
	// the policy-bound media client for the third-party request, so FFmpeg
	// never receives Plex tokens and does not have to reproduce the source
	// host's redirect headers.
	return s.hls.register(hlsSessionIDFromRequest(request), mapping.ResolvedURL, s.localPartProxyURL(mapping.PartID), request.URL.Query(), audioTranscode, forceTranscode, format, videoCodecString, audioStreamIndex)
}

// localPartProxyURL builds the loopback URL that in-process media consumers
// (ffprobe, ffmpeg, the HLS downloader) use to fetch a protected Part. The
// host mirrors the configured listener: a wildcard bind still reaches the
// process via loopback, while an explicit interface bind must be reused
// because loopback would not be served.
func (s *Server) localPartProxyURL(partID string) string {
	return s.localProxyURL("/library/parts/"+url.PathEscape(partID)+"/file", "plexTranscode=1")
}

func (s *Server) localProxyHostPort() (string, string) {
	host := "127.0.0.1"
	port := "3001"
	if listenHost, listenPort, err := net.SplitHostPort(s.cfg.ListenAddr); err == nil {
		if listenPort != "" {
			port = listenPort
		}
		switch listenHost {
		case "", "0.0.0.0", "::", "[::]":
			// Wildcard listener: loopback is always reachable in-process.
		default:
			host = listenHost
		}
	}
	return host, port
}

func (s *Server) localPartProxyAvailable() bool {
	_, port, err := net.SplitHostPort(strings.TrimSpace(s.cfg.ListenAddr))
	return err == nil && port != "" && port != "0"
}

func (s *Server) handleProxyTranscodeStart(writer http.ResponseWriter, request *http.Request, sessionID string) {
	manifest, err := s.hls.start(context.WithoutCancel(request.Context()), sessionID)
	if err != nil {
		s.logger.Warn("failed to start STRM proxy transcode", "request_id", requestID(request.Context()), "session", sessionID, "error", err)
		writeJSONError(writer, http.StatusBadGateway, "STRM proxy transcode unavailable")
		return
	}
	format, _ := s.hls.sessionFormat(sessionID)
	contentType := "application/vnd.apple.mpegurl"
	if format == proxyTranscodeDASH {
		contentType = "application/dash+xml"
	}
	setHLSResponseHeaders(writer, request, contentType)
	writer.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(writer, manifest)
}

func (s *Server) handleTranscodeResource(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writeJSONError(writer, http.StatusMethodNotAllowed, "transcode resource supports GET and HEAD only")
		return
	}
	sessionID, relative, ok := parseHLSResourcePath(request.URL.Path)
	if !ok || s.hls == nil {
		s.proxyToPlex(writer, request, "")
		return
	}
	session, sessionOK := s.hls.session(sessionID)
	if !sessionOK || !isProxyOwnedTranscodeResource(session, relative) {
		// Unknown resources under the Plex session prefix may belong to Plex's
		// own transcode session. Only claim files produced by this proxy.
		s.proxyToPlex(writer, request, "")
		return
	}
	filename, ok := s.hls.filePath(sessionID, relative)
	if !ok {
		s.proxyToPlex(writer, request, "")
		return
	}
	if _, err := os.Stat(filename); err != nil {
		format, _ := s.hls.sessionFormat(sessionID)
		if strings.HasSuffix(strings.ToLower(relative), ".m3u8") || strings.HasSuffix(strings.ToLower(relative), ".mpd") {
			if _, err := s.hls.start(request.Context(), sessionID); err != nil {
				s.logger.Warn("failed to start STRM proxy transcode from manifest request", "request_id", requestID(request.Context()), "session", sessionID, "format", format, "error", err)
				writeJSONError(writer, http.StatusBadGateway, "STRM proxy transcode unavailable")
				return
			}
		}
		if format == proxyTranscodeHLS {
			if segmentIndex, segmentOK := parseHLSSegmentIndex(relative); segmentOK {
				s.hls.ensureSegment(sessionID, segmentIndex)
			}
		}
		if err := waitForHLSFile(request.Context(), filename, session); err != nil {
			writeJSONError(writer, http.StatusNotFound, "HLS resource is not ready")
			return
		}
	}
	contentType := "application/octet-stream"
	if strings.HasSuffix(strings.ToLower(filename), ".m3u8") {
		contentType = "application/vnd.apple.mpegurl"
	} else if strings.HasSuffix(strings.ToLower(filename), ".mpd") {
		contentType = "application/dash+xml"
	} else if strings.HasSuffix(strings.ToLower(filename), ".ts") {
		contentType = "video/mp2t"
	} else if strings.HasSuffix(strings.ToLower(filename), ".m4s") || strings.HasSuffix(strings.ToLower(filename), ".mp4") {
		contentType = "video/mp4"
	}
	setHLSResponseHeaders(writer, request, contentType)
	http.ServeFile(writer, request, filename)
}

func setHLSResponseHeaders(writer http.ResponseWriter, request *http.Request, contentType string) {
	setMediaCORSHeaders(writer, request)
	writer.Header().Set("Content-Type", contentType)
	writer.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
}

func writeHLSTranscodeDecision(writer http.ResponseWriter, request *http.Request, mapping PartMapping, metadataContentType string, metadataBody []byte, sessionID string) {
	writeProxyTranscodeDecision(writer, request, mapping, metadataContentType, metadataBody, sessionID, proxyTranscodeHLS, defaultProxyStreamPlan())
}

func writeProxyTranscodeDecision(writer http.ResponseWriter, request *http.Request, mapping PartMapping, metadataContentType string, metadataBody []byte, sessionID string, format proxyTranscodeFormat, plan proxyStreamPlan) {
	query := request.URL.Query()
	width, height := parseVideoResolution(query.Get("videoResolution"))
	if width <= 0 || height <= 0 {
		width, height = 1280, 720
	}
	bitrate := parseBitrateKbps(query.Get("maxVideoBitrate"))
	metadataPath := query.Get("path")
	if metadataPath == "" {
		metadataPath = "/library/metadata/" + mapping.PartID
	}
	writer.Header().Set("Cache-Control", "no-cache")
	proxyHeader := "hls-transcode"
	if format == proxyTranscodeDASH {
		proxyHeader = "dash-transcode"
	}
	writer.Header().Set("X-Plex-Strm-Proxy", proxyHeader)
	setMediaCORSHeaders(writer, request)
	if isJSONContentType(metadataContentType) {
		if rewritten, changed, err := renderProxyTranscodeDecisionJSONWithPlan(metadataBody, mapping, query, sessionID, format, plan); err == nil && changed {
			writer.Header().Set("Content-Type", "application/json; charset=utf-8")
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write(rewritten)
			return
		}
	}
	writer.Header().Set("Content-Type", "application/xml; charset=utf-8")
	if rewritten, changed, err := renderProxyTranscodeDecisionXMLWithPlan(metadataBody, metadataContentType, mapping, query, sessionID, format, plan); err == nil && changed {
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(rewritten)
		return
	}
	container, protocol, partKey := proxyDecisionMediaAttributes(format, sessionID)
	videoCodec := plan.VideoCodec
	if videoCodec == "" {
		videoCodec = "h264"
	}
	audioCodec := plan.AudioCodec
	if audioCodec == "" {
		audioCodec = "aac"
	}
	_, _ = fmt.Fprintf(writer, `<?xml version="1.0" encoding="UTF-8"?>
<MediaContainer size="1" allowSync="1" directPlayDecisionCode="3000" directPlayDecisionText="App cannot direct play this item. Direct play is disabled." generalDecisionCode="1001" generalDecisionText="Direct play not available; Conversion OK." identifier="com.plexapp.plugins.library" resourceSession="%s" transcodeDecisionCode="1001" transcodeDecisionText="Direct play not available; Conversion OK.">
<Video key="%s" type="movie"><Media id="%s" bitrate="%d" container="%s" protocol="%s" videoCodec="%s" audioCodec="%s" width="%d" height="%d" selected="1"><Part id="%s" key="%s" container="%s" protocol="%s" decision="transcode" width="%d" height="%d" selected="1"><Stream streamType="1" codec="%s" width="%d" height="%d" decision="%s" location="%s" /><Stream streamType="2" codec="%s" channels="2" decision="%s" location="%s" /></Part></Media></Video>
</MediaContainer>`, sessionID, metadataPath, mapping.PartID, bitrate, container, protocol, videoCodec, audioCodec, width, height, mapping.PartID, partKey, container, protocol, width, height, videoCodec, width, height, plan.VideoDecision, plan.VideoLocation, audioCodec, plan.AudioDecision, plan.AudioLocation)
}

func writeCachedDirectDecision(writer http.ResponseWriter, request *http.Request, decision directDecision) {
	contentType := decision.contentType
	if strings.TrimSpace(contentType) == "" {
		contentType = "application/xml; charset=utf-8"
	}
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Content-Type", contentType)
	writer.Header().Set("Content-Length", strconv.Itoa(len(decision.body)))
	writer.Header().Set("X-Plex-Strm-Proxy", "direct-cache")
	setMediaCORSHeaders(writer, request)
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(decision.body)
}

func (s *Server) handlePartFile(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodOptions {
		setMediaCORSHeaders(writer, request)
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	partID, ok := parsePartID(request.URL.Path)
	if !ok {
		s.proxyToPlex(writer, request, "")
		return
	}
	mapping, found := s.mappings.Get(partID)
	if !found {
		s.logger.Warn("part mapping unavailable", "request_id", requestID(request.Context()), "part_id", partID)
		if !s.cfg.StrictPartMap {
			s.proxyToPlex(writer, request, "")
			return
		}
		writeJSONError(writer, http.StatusNotFound, "Plex part mapping is not available; retry playback decision")
		return
	}
	if mapping.Kind == PartKindLocal {
		s.proxyToPlex(writer, request, "")
		return
	}
	if mapping.ResolvedURL == "" {
		s.logger.Error("STRM part has no resolved URL", "request_id", requestID(request.Context()), "part_id", partID, "resolution_error", mapping.ResolutionErr)
		writeJSONError(writer, http.StatusBadGateway, "STRM URL could not be resolved")
		return
	}
	target, err := url.Parse(mapping.ResolvedURL)
	if err != nil {
		writeJSONError(writer, http.StatusBadGateway, "STRM URL is invalid")
		return
	}
	if err := s.policy.ValidateURL(request.Context(), target); err != nil {
		s.logger.Warn("media target rejected", "request_id", requestID(request.Context()), "part_id", partID, "target_host", target.Hostname(), "error", err)
		writeJSONError(writer, http.StatusBadGateway, "media target is not allowed")
		return
	}
	if request.URL.Query().Get("plexTranscode") == "1" {
		s.proxyMedia(writer, request, target)
		return
	}

	plan, _ := s.selectPlaybackPlan(request, mapping, PlaybackStageMetadata, PlexDecisionDirectPlay, true)
	s.logger.Debug("serving STRM Part according to playback policy", "request_id", requestID(request.Context()), "part_id", mapping.PartID, "plan", plan.Plan, "reason", plan.Reason)
	s.serveResolvedMedia(writer, request, mapping)
}

func (s *Server) serveResolvedMedia(writer http.ResponseWriter, request *http.Request, mapping PartMapping) {
	if s.cfg.PlaybackMode == "proxy" || s.cfg.ProxyFallback {
		target, err := url.Parse(mapping.ResolvedURL)
		if err != nil {
			writeJSONError(writer, http.StatusBadGateway, "STRM URL is invalid")
			return
		}
		s.proxyMedia(writer, request, target)
		return
	}
	writer.Header().Set("Location", mapping.ResolvedURL)
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Plex-Strm-Proxy", "redirect")
	writer.WriteHeader(s.cfg.RedirectStatus)
}

func (s *Server) proxyToPlex(writer http.ResponseWriter, request *http.Request, defaultSTRMPath string) {
	if defaultSTRMPath != "" {
		request = request.WithContext(context.WithValue(request.Context(), defaultSTRMKey, defaultSTRMPath))
	}
	s.plexProxy.ServeHTTP(writer, request)
}

// proxyToPlexBuffered is used only for the small decision/start responses.
// Buffering lets the STRM path try Plex's native flow first and choose the
// proxy HLS fallback only when Plex actually rejects that flow. Ordinary Plex
// resources and media remain fully streaming through ReverseProxy.
func (s *Server) proxyToPlexBuffered(request *http.Request, defaultSTRMPath string) (int, http.Header, []byte, error) {
	capture := &plexProxyErrorCapture{}
	request = request.WithContext(context.WithValue(request.Context(), plexProxyErrorKey, capture))
	recorder := httptest.NewRecorder()
	s.proxyToPlex(recorder, request, defaultSTRMPath)
	response := recorder.Result()
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, s.cfg.MaxPlexBodyBytes+1))
	if err != nil {
		return http.StatusBadGateway, make(http.Header), nil, err
	}
	return response.StatusCode, response.Header.Clone(), body, capture.get()
}

func writeBufferedPlexResponse(writer http.ResponseWriter, status int, headers http.Header, body []byte) {
	for name, values := range headers {
		writer.Header()[name] = append([]string(nil), values...)
	}
	if status <= 0 {
		status = http.StatusBadGateway
	}
	writer.WriteHeader(status)
	if len(body) > 0 {
		_, _ = writer.Write(body)
	}
}

func (s *Server) proxyMedia(writer http.ResponseWriter, request *http.Request, target *url.URL) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writeJSONError(writer, http.StatusMethodNotAllowed, "media endpoint supports GET and HEAD only")
		return
	}
	outgoing, err := http.NewRequestWithContext(request.Context(), request.Method, target.String(), request.Body)
	if err != nil {
		writeJSONError(writer, http.StatusBadGateway, "failed to create media request")
		return
	}
	copyMediaRequestHeaders(outgoing.Header, request.Header)
	response, err := s.mediaClient.Do(outgoing)
	if err != nil {
		s.logger.Warn("media upstream request failed", "request_id", requestID(request.Context()), "target_host", target.Hostname(), "error", err)
		writeJSONError(writer, http.StatusBadGateway, "media upstream unavailable")
		return
	}
	defer response.Body.Close()
	prefix := []byte(nil)
	if request.Method == http.MethodGet {
		buffer := make([]byte, 64)
		count, readErr := response.Body.Read(buffer)
		if readErr != nil && readErr != io.EOF {
			s.logger.Warn("media upstream prefix read failed", "request_id", requestID(request.Context()), "target_host", target.Hostname(), "error", readErr)
			writeJSONError(writer, http.StatusBadGateway, "media upstream unavailable")
			return
		}
		prefix = buffer[:count]
	}
	copyMediaResponseHeaders(writer.Header(), response.Header)
	// Plex's internal DASH transcode fetch treats an unbounded 200 response's
	// huge Content-Length as an allocation hint. Keep ordinary media and Range
	// responses transparent, but let the protected Part route stream an
	// unbounded source instead of advertising the whole remote file size.
	if request.URL.Query().Get("plexTranscode") == "1" && request.Header.Get("Range") == "" && response.StatusCode == http.StatusOK {
		writer.Header().Del("Content-Length")
	}
	setMediaCORSHeaders(writer, request)
	if detectedType := detectMediaContentType(prefix, target); detectedType != "" {
		writer.Header().Set("Content-Type", detectedType)
	}
	writer.WriteHeader(response.StatusCode)
	if request.Method == http.MethodHead {
		return
	}
	if len(prefix) > 0 {
		if _, err := writer.Write(prefix); err != nil {
			s.logger.Warn("media response stream interrupted", "request_id", requestID(request.Context()), "target_host", target.Hostname(), "error", err)
			return
		}
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
	}
	if err := streamMediaBody(writer, response.Body); err != nil {
		s.logger.Warn("media response stream interrupted", "request_id", requestID(request.Context()), "target_host", target.Hostname(), "error", err)
	}
}

func setMediaCORSHeaders(writer http.ResponseWriter, request *http.Request) {
	origin := strings.TrimSpace(request.Header.Get("Origin"))
	if origin == "" {
		writer.Header().Set("Access-Control-Allow-Origin", "*")
	} else {
		writer.Header().Set("Access-Control-Allow-Origin", origin)
		writer.Header().Add("Vary", "Origin")
	}
	writer.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	writer.Header().Set("Access-Control-Allow-Headers", "Range, If-Range, If-None-Match, If-Modified-Since, Accept, Content-Type")
	writer.Header().Set("Access-Control-Expose-Headers", "Accept-Ranges, Cache-Control, Content-Disposition, Content-Length, Content-Range, Content-Type, ETag, Last-Modified, Location, Retry-After")
}

func detectMediaContentType(prefix []byte, target *url.URL) string {
	if len(prefix) >= 4 && bytes.Equal(prefix[:4], []byte{0x1a, 0x45, 0xdf, 0xa3}) {
		return "video/x-matroska"
	}
	if len(prefix) >= 12 && string(prefix[4:8]) == "ftyp" {
		return "video/mp4"
	}
	if len(prefix) >= 12 && string(prefix[:4]) == "RIFF" && string(prefix[8:12]) == "AVI " {
		return "video/x-msvideo"
	}
	if target != nil {
		switch strings.TrimPrefix(strings.ToLower(path.Ext(target.Path)), ".") {
		case "mkv", "mka":
			return "video/x-matroska"
		case "mp4", "m4v", "mov":
			return "video/mp4"
		case "webm":
			return "video/webm"
		}
	}
	return ""
}

func streamMediaBody(writer http.ResponseWriter, source io.Reader) error {
	buffer := make([]byte, 32*1024)
	for {
		count, readErr := source.Read(buffer)
		if count > 0 {
			if _, writeErr := writer.Write(buffer[:count]); writeErr != nil {
				return writeErr
			}
			if flusher, ok := writer.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func (s *Server) modifyPlexResponse(response *http.Response) error {
	publicHost := ""
	if response.Request != nil {
		publicHost = response.Request.Host
	}
	rewritePlexLocation(response.Header, s.cfg.PlexUpstream, publicHost)
	if !isStructuredContentType(response.Header.Get("Content-Type")) || response.Body == nil {
		return nil
	}
	if response.ContentLength > s.cfg.MaxPlexBodyBytes {
		return nil
	}
	defaultSTRMPath := ""
	responseRequestID := ""
	if response.Request != nil {
		defaultSTRMPath, _ = response.Request.Context().Value(defaultSTRMKey).(string)
		responseRequestID = requestID(response.Request.Context())
	}
	contentType := response.Header.Get("Content-Type")
	contentEncoding := response.Header.Get("Content-Encoding")
	if response.Request != nil && isDecisionRequest(response.Request) {
		source := response.Body
		rawBody, err := io.ReadAll(io.LimitReader(source, s.cfg.MaxPlexBodyBytes+1))
		_ = source.Close()
		if err != nil {
			return fmt.Errorf("read Plex decision response: %w", err)
		}
		if int64(len(rawBody)) > s.cfg.MaxPlexBodyBytes {
			response.Body = io.NopCloser(bytes.NewReader(rawBody))
			return nil
		}
		decoded, err := decodeStructuredBody(contentEncoding, rawBody, s.cfg.MaxPlexBodyBytes)
		if err != nil {
			return fmt.Errorf("decode Plex decision response: %w", err)
		}
		nativeSTRM := isNativeSTRMRequest(response.Request)
		if !nativeSTRM {
			// A native STRM decision is evaluated against the proxy-local metadata
			// Part. Its response therefore commonly contains a local/proxy Part
			// representation (for example file="127.0.0.1"), which is not an
			// authoritative library mapping and must not replace the STRM mapping
			// established from Plex's original metadata response.
			s.ingestPlexStructuredResponse(responseRequestID, contentType, decoded, defaultSTRMPath)
		}
		upstreamSummary, upstreamSummaryOK := summarizeDecisionResponse(contentType, decoded)
		s.logger.Info("STRM decision response captured", "request_id", responseRequestID, "phase", "upstream", "content_type", contentType, "decoded_bytes", len(decoded), "summary_ok", upstreamSummaryOK, "summary", upstreamSummary)
		responseBody := rawBody
		if mapping, ok := decisionResponsePartMapping(s.mappings, contentType, decoded); ok {
			if nativeSTRM {
				// Plex made this decision using the proxy-local metadata/Part. Do
				// not run the proxy playback policy over Plex's result: that would
				// turn a native stream-level decision back into a codec-specific
				// proxy decision. Only replace a direct result's local Part URL so
				// the client can follow the usual direct/302 path.
				directURLChanged := false
				if plexDecisionFromBody(contentType, decoded) == PlexDecisionDirectPlay {
					var rewritten []byte
					var changed bool
					switch {
					case isXMLContentType(contentType):
						rewritten, changed, err = rewriteSTRMNativeDirectDecisionXML(decoded, mapping)
					case isJSONContentType(contentType):
						rewritten, changed, err = rewriteSTRMNativeDirectDecisionJSON(decoded, mapping)
					}
					if err != nil {
						return fmt.Errorf("rewrite native Plex STRM decision: %w", err)
					}
					if changed {
						responseBody, err = encodeStructuredBody(contentEncoding, rewritten)
						if err != nil {
							return fmt.Errorf("encode native Plex STRM decision: %w", err)
						}
						response.Header.Set("Content-Length", fmt.Sprintf("%d", len(responseBody)))
						response.ContentLength = int64(len(responseBody))
					}
					directURLChanged = changed
				}
				s.logger.Info("preserving native Plex STRM decision", "request_id", responseRequestID, "part_id", mapping.PartID, "decision", plexDecisionFromBody(contentType, decoded), "direct_url_rewritten", directURLChanged)
			} else {
				plan, probe := s.selectPlaybackPlan(response.Request, mapping, PlaybackStageDecisionResponse, plexDecisionFromBody(contentType, decoded), false)
				var rewritten []byte
				var changed bool
				if plan.Plan == PlaybackPlanProxyHLSAudioFallback || plan.Plan == PlaybackPlanPlexTranscode {
					audioCodec := ""
					if probe != nil {
						audioCodec = probe.AudioCodec
					}
					s.logger.Info("preserving Plex decision for STRM fallback", "request_id", responseRequestID, "part_id", mapping.PartID, "audio_codec", audioCodec, "plan", plan.Plan, "reason", plan.Reason)
					rewritten = decoded
				} else {
					switch {
					case isXMLContentType(contentType):
						rewritten, changed, err = rewriteSTRMDirectDecisionXMLWithProbe(decoded, mapping, probe)
					case isJSONContentType(contentType):
						rewritten, changed, err = rewriteSTRMDirectDecisionJSONWithProbe(decoded, mapping, probe)
					}
				}
				if err != nil {
					return fmt.Errorf("rewrite Plex STRM decision: %w", err)
				}
				if changed {
					responseBody, err = encodeStructuredBody(contentEncoding, rewritten)
					if err != nil {
						return fmt.Errorf("encode Plex STRM decision: %w", err)
					}
					response.Header.Set("Content-Length", fmt.Sprintf("%d", len(responseBody)))
					response.ContentLength = int64(len(responseBody))
				}
			}
		}
		if decodedDirect, decodeErr := decodeStructuredBody(contentEncoding, responseBody, s.cfg.MaxPlexBodyBytes); decodeErr == nil {
			returnedSummary, returnedSummaryOK := summarizeDecisionResponse(contentType, decodedDirect)
			s.logger.Info("STRM decision response captured", "request_id", responseRequestID, "phase", "returned", "content_type", contentType, "decoded_bytes", len(decodedDirect), "summary_ok", returnedSummaryOK, "summary", returnedSummary)
			s.rememberDirectDecision(response.Request, contentType, decodedDirect)
		}
		response.Body = io.NopCloser(bytes.NewReader(responseBody))
		return nil
	}
	if response.Request != nil && isMetadataResponseRequest(response.Request) {
		source := response.Body
		body, err := io.ReadAll(io.LimitReader(source, s.cfg.MaxPlexBodyBytes+1))
		_ = source.Close()
		if err != nil {
			return fmt.Errorf("read Plex metadata response: %w", err)
		}
		if int64(len(body)) > s.cfg.MaxPlexBodyBytes {
			response.Body = io.NopCloser(bytes.NewReader(body))
			return nil
		}
		decoded, err := decodeStructuredBody(contentEncoding, body, s.cfg.MaxPlexBodyBytes)
		if err != nil {
			return fmt.Errorf("decode Plex metadata response: %w", err)
		}
		s.ingestPlexStructuredResponse(responseRequestID, contentType, decoded, defaultSTRMPath)
		s.rememberDirectDecision(response.Request, contentType, decoded)
		s.rememberMetadataPartLookups(response.Request.URL.Path, contentType, decoded)
		// Plex Web uses the declared container to choose the playback branch.
		// Keep the STRM compatibility declaration that selects direct play; the
		// media handler below corrects the actual Content-Type from the bytes.
		probeMedia := response.Request != nil && response.Request.URL != nil && response.Request.URL.Query().Get("plexTranscode") == "1"
		if response.Request != nil && isDetailedMetadataRequest(response.Request) {
			probeMedia = true
		}
		rewritten, changed, err := s.rewriteSTRMMetadataResponse(response.Request, contentType, decoded, probeMedia)
		if err != nil {
			return fmt.Errorf("rewrite Plex STRM metadata: %w", err)
		}
		upstreamSummary, upstreamSummaryOK := summarizeDecisionResponse(contentType, decoded)
		returnedSummary, returnedSummaryOK := summarizeDecisionResponse(contentType, rewritten)
		s.logger.Info("Plex metadata response inspected", "request_id", responseRequestID, "path", response.Request.URL.Path, "accept", response.Request.Header.Get("Accept"), "content_type", contentType, "content_encoding", contentEncoding, "decoded_bytes", len(decoded), "rewritten_bytes", len(rewritten), "changed", changed, "probe_media", probeMedia, "upstream_summary_ok", upstreamSummaryOK, "upstream_summary", upstreamSummary, "returned_summary_ok", returnedSummaryOK, "returned_summary", returnedSummary)
		if changed {
			body, err = encodeStructuredBody(contentEncoding, rewritten)
			if err != nil {
				return fmt.Errorf("encode Plex metadata response: %w", err)
			}
			response.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
			response.ContentLength = int64(len(body))
		}
		response.Body = io.NopCloser(bytes.NewReader(body))
		return nil
	}
	response.Body = &captureReadCloser{
		source: response.Body,
		limit:  s.cfg.MaxPlexBodyBytes,
		onDone: func(body []byte) {
			decoded, err := decodeStructuredBody(contentEncoding, body, s.cfg.MaxPlexBodyBytes)
			if err != nil {
				s.logger.Warn("failed to decode Plex structured response", "request_id", responseRequestID, "content_encoding", contentEncoding, "error", err)
				return
			}
			mappings := s.mappings.IngestStructuredResponse(contentType, decoded, defaultSTRMPath, s.resolver)
			if len(mappings) > 0 {
				s.logger.Debug("indexed Plex part mappings", "request_id", responseRequestID, "count", len(mappings))
			}
		},
	}
	return nil
}

func isMetadataResponseRequest(request *http.Request) bool {
	if request == nil || request.URL == nil {
		return false
	}
	if _, ok := parseMetadataPath(request.URL.Path); ok {
		return true
	}
	if isDecisionRequest(request) || isTranscodeStartRequest(request) {
		return true
	}
	// Plex Web frequently starts playback from the metadata embedded in a Hub
	// response (for example, Continue Watching) instead of fetching the full
	// metadata endpoint again. Rewrite those responses too, otherwise the STRM
	// Part has no container and Plex Web falls back to DASH transcoding.
	return strings.HasPrefix(request.URL.Path, "/hubs/") || strings.HasPrefix(request.URL.Path, "/library/sections/") || strings.HasPrefix(request.URL.Path, "/playQueues")
}

func isNativeSTRMRequest(request *http.Request) bool {
	if request == nil {
		return false
	}
	native, _ := request.Context().Value(nativeSTRMKey).(bool)
	return native
}

func isDetailedMetadataRequest(request *http.Request) bool {
	if request == nil || request.URL == nil {
		return false
	}
	_, ok := parseMetadataPath(request.URL.Path)
	return ok
}

func isPlayQueueMetadataRequest(request *http.Request) bool {
	return request != nil && request.URL != nil && strings.HasPrefix(request.URL.Path, "/playQueues")
}

func (s *Server) ingestPlexStructuredResponse(requestID, contentType string, body []byte, defaultSTRMPath string) {
	mappings := s.mappings.IngestStructuredResponse(contentType, body, defaultSTRMPath, s.resolver)
	if len(mappings) > 0 {
		s.logger.Debug("indexed Plex part mappings", "request_id", requestID, "count", len(mappings))
	}
}

func (s *Server) rewriteSTRMMetadataResponse(source *http.Request, contentType string, body []byte, probeMedia bool) ([]byte, bool, error) {
	ctx := context.Background()
	if source != nil {
		ctx = source.Context()
	}
	transcodeMetadata := source != nil && source.URL != nil && source.URL.Query().Get("plexTranscode") == "1"
	directAllowed := func(part partRecord) bool {
		mapping, ok := s.mappings.Get(part.ID)
		if !ok || mapping.Kind != PartKindSTRM || mapping.ResolvedURL == "" {
			return false
		}
		request := source
		if request == nil {
			request, _ = http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/", nil)
		}
		plan, _ := s.selectPlaybackPlan(request, mapping, PlaybackStageMetadata, PlexDecisionUnknown, false)
		return plan.Plan == PlaybackPlanSTRMRedirect
	}
	containerFor := func(part partRecord) string {
		if !directAllowed(part) {
			return ""
		}
		mapping, ok := s.mappings.Get(part.ID)
		if !ok || mapping.Kind != PartKindSTRM {
			return ""
		}
		return mediaContainerForURL(mapping.ResolvedURL)
	}
	fileFor := func(part partRecord) string {
		if !directAllowed(part) {
			return ""
		}
		mapping, ok := s.mappings.Get(part.ID)
		if !ok || mapping.Kind != PartKindSTRM {
			return ""
		}
		if transcodeMetadata {
			return s.partProxyURL(source, mapping.PartID)
		}
		return mapping.ResolvedURL
	}
	// Home hubs and library lists must never start a remote request or wait for
	// FFprobe. A single-media detail request is different: it is the request
	// made after the user selected an item, and is the last metadata response
	// before Plex clients build their playback decision. Probe that one item so
	// the client receives real codec and Stream information before it sends
	// directPlay/directStream. For play queues, restrict a cold probe to the
	// selected Part; cached probes can still enrich any other STRM entries.
	selectedPartID := ""
	if probeMedia && source != nil && source.URL != nil {
		selectedPartID = strings.TrimSpace(source.URL.Query().Get("plexPartID"))
	}
	detailRequest := isDetailedMetadataRequest(source)
	queueRequest := isPlayQueueMetadataRequest(source)
	selectedQueueParts := make(map[string]bool)
	if queueRequest {
		selectedQueueParts = selectedPlayQueuePartIDs(contentType, body)
	}
	detailSTRMParts := 0
	if detailRequest {
		for _, part := range extractPartRecords(contentType, body) {
			mapping, ok := s.mappings.Get(part.ID)
			if ok && mapping.Kind == PartKindSTRM && mapping.ResolvedURL != "" {
				detailSTRMParts++
			}
		}
	}
	// A metadata endpoint can represent a show/season with many episodes. Do
	// not probe the first Part merely because the URL contains
	// /library/metadata/. A cold detail probe is safe only for one selected
	// STRM Part, an explicit internal plexPartID, or a queue's selected item.
	detailProbeAllowed := detailRequest && (selectedPartID != "" || detailSTRMParts == 1)
	coldProbeAllowed := detailProbeAllowed || (queueRequest && len(selectedQueueParts) > 0) || (probeMedia && selectedPartID != "")
	coldProbeUsed := false
	probeFor := func(part partRecord) *mediaProbe {
		if selectedPartID != "" && part.ID != selectedPartID {
			return nil
		}
		if queueRequest && len(selectedQueueParts) > 0 && !selectedQueueParts[part.ID] {
			return nil
		}
		if !directAllowed(part) {
			return nil
		}
		mapping, ok := s.mappings.Get(part.ID)
		if !ok || mapping.Kind != PartKindSTRM || mapping.ResolvedURL == "" {
			return nil
		}
		probe, ok := s.cachedSTRMMediaProbeForMapping(mapping)
		if !ok {
			if !coldProbeAllowed || coldProbeUsed {
				return nil
			}
			coldProbeUsed = true
			probe, ok = s.probeSTRMMediaForMetadata(ctx, mapping)
			if !ok {
				return nil
			}
		}
		return &probe
	}
	return rewriteSTRMMetadataWithContainerAndFileAndProbe(contentType, body, containerFor, fileFor, probeFor)
}

func mediaContainerForURL(sourceURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(sourceURL))
	if err != nil {
		return ""
	}
	container := strings.TrimPrefix(strings.ToLower(path.Ext(parsed.Path)), ".")
	switch container {
	case "mkv", "mp4", "m4v", "mov", "webm", "avi":
		return container
	default:
		return ""
	}
}

// directDecisionSessionLimit bounds how many remembered direct decisions
// (which include full response bodies) are kept at once.
const directDecisionSessionLimit = 128

func (s *Server) rememberDirectDecision(request *http.Request, contentType string, body []byte) {
	if request == nil || !isDecisionRequest(request) {
		return
	}
	sessionID := hlsSessionIDFromRequest(request)
	if sessionID == "" {
		return
	}
	// Every decision response supersedes the previous result for this playback
	// transaction. In particular, a native transcode response must invalidate
	// an earlier direct result before start.m3u8/start.mpd is handled.
	s.clearDirectDecision(sessionID)
	if !decisionBodyIsDirect(contentType, body) {
		return
	}
	for _, record := range extractPartRecords(contentType, body) {
		for _, partID := range mappingPartIDs(record) {
			mapping, ok := s.mappings.Get(partID)
			if !ok || mapping.Kind != PartKindSTRM || mapping.ResolvedURL == "" {
				continue
			}
			plan, _ := s.selectPlaybackPlan(request, mapping, PlaybackStageDecisionResponse, PlexDecisionDirectPlay, false)
			if plan.Plan != PlaybackPlanSTRMRedirect {
				continue
			}
			s.decisionMu.Lock()
			now := time.Now()
			metadataPath, _ := parseMetadataPath(request.URL.Query().Get("path"))
			decision := directDecision{mapping: mapping, metadataPath: metadataPath, contentType: contentType, body: append([]byte(nil), body...), createdAt: now, expiresAt: now.Add(s.cfg.MappingCacheTTL)}
			s.directDecisions[sessionID] = decision
			s.pruneDirectDecisionsLocked(now)
			s.decisionMu.Unlock()
			s.logger.Info("remembered direct STRM decision", "request_id", requestID(request.Context()), "session", sessionID, "part_id", mapping.PartID, "target_host", resolvedTargetHost(mapping.ResolvedURL))
			return
		}
	}
}

func (s *Server) clearDirectDecision(sessionID string) {
	if strings.TrimSpace(sessionID) == "" {
		return
	}
	s.decisionMu.Lock()
	delete(s.directDecisions, sessionID)
	s.decisionMu.Unlock()
}

// pruneDirectDecisionsLocked drops expired records from both direct-decision
// views and, while a view still exceeds directDecisionSessionLimit, evicts
// the oldest entries so the caches stay bounded.
func (s *Server) pruneDirectDecisionsLocked(now time.Time) {
	for sessionID, decision := range s.directDecisions {
		if now.After(decision.expiresAt) {
			delete(s.directDecisions, sessionID)
		}
	}
	for len(s.directDecisions) > directDecisionSessionLimit {
		oldestSession := ""
		var oldestAt time.Time
		for sessionID, decision := range s.directDecisions {
			if oldestSession == "" || decision.createdAt.Before(oldestAt) {
				oldestSession, oldestAt = sessionID, decision.createdAt
			}
		}
		delete(s.directDecisions, oldestSession)
	}
}

func (s *Server) directDecisionForStart(sessionID, partID string) (PartMapping, bool) {
	decision, ok := s.directDecisionRecordForStart(sessionID, partID)
	if !ok {
		return PartMapping{}, false
	}
	return decision.mapping, true
}

func (s *Server) directDecisionRecordForStart(sessionID, partID string) (directDecision, bool) {
	decision, ok := s.directDecisionRecord(sessionID)
	if !ok {
		return directDecision{}, false
	}
	if partID != "" && decision.mapping.PartID != partID {
		return directDecision{}, false
	}
	return decision, true
}

func (s *Server) directDecision(sessionID string) (PartMapping, bool) {
	decision, ok := s.directDecisionRecord(sessionID)
	if !ok {
		return PartMapping{}, false
	}
	return decision.mapping, true
}

func (s *Server) directDecisionRecord(sessionID string) (directDecision, bool) {
	if sessionID == "" {
		return directDecision{}, false
	}
	s.decisionMu.Lock()
	defer s.decisionMu.Unlock()
	decision, ok := s.directDecisions[sessionID]
	if !ok {
		return directDecision{}, false
	}
	if time.Now().After(decision.expiresAt) {
		delete(s.directDecisions, sessionID)
		return directDecision{}, false
	}
	return decision, true
}

func decisionBodyIsDirect(contentType string, body []byte) bool {
	return plexDecisionFromBody(contentType, body) == PlexDecisionDirectPlay
}

func rewritePlexLocation(headers http.Header, upstream *url.URL, publicHost string) {
	location := headers.Get("Location")
	if location == "" || upstream == nil {
		return
	}

	parsed, err := url.Parse(location)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return
	}
	isUpstreamLocation := strings.EqualFold(parsed.Scheme, upstream.Scheme) && strings.EqualFold(parsed.Host, upstream.Host)
	isPublicLocation := publicHost != "" && strings.EqualFold(parsed.Host, publicHost)
	if !isUpstreamLocation && !isPublicLocation {
		return
	}

	relative := parsed.EscapedPath()
	if relative == "" {
		relative = "/"
	}
	if parsed.RawQuery != "" {
		relative += "?" + parsed.RawQuery
	}
	if parsed.Fragment != "" {
		relative += "#" + parsed.Fragment
	}
	headers.Set("Location", relative)
}

func newPlexTransport(headerTimeout time.Duration) http.RoundTripper {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return http.DefaultTransport
	}
	transport := base.Clone()
	transport.ResponseHeaderTimeout = headerTimeout
	return transport
}

var noRedirects = func(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

func isDecisionRequest(request *http.Request) bool {
	return request.URL.Path == "/video/:/transcode/universal/decision"
}

func isTranscodeStartRequest(request *http.Request) bool {
	const prefix = "/video/:/transcode/universal/start"
	if !strings.HasPrefix(request.URL.Path, prefix) {
		return false
	}
	return len(request.URL.Path) == len(prefix) || request.URL.Path[len(prefix)] == '.' || request.URL.Path[len(prefix)] == '/'
}

func isHLSPlayback(query url.Values) bool {
	return strings.EqualFold(strings.TrimSpace(query.Get("protocol")), "hls")
}

func isHLSStartRequest(request *http.Request) bool {
	if request == nil || request.URL == nil {
		return false
	}
	return isHLSPlayback(request.URL.Query()) || strings.HasSuffix(strings.ToLower(request.URL.Path), ".m3u8")
}

func prepareSTRMPlaybackQuery(request *http.Request) url.Values {
	query := request.URL.Query()
	if isHLSPlayback(query) {
		query.Set("directPlay", "0")
		query.Set("directStream", "0")
		return query
	}
	query.Set("directPlay", "1")
	return query
}

func prepareSTRMDirectPlaybackQuery(request *http.Request, sourceURL string) url.Values {
	query := request.URL.Query()
	if strings.TrimSpace(sourceURL) != "" {
		query.Set("path", sourceURL)
	}
	// HLS is a client playback preference, not the media's transport type.
	// Ask Plex to try its generic HTTP direct-play path first; if that is not
	// usable, the client will request start.m3u8 and the HLS fallback handles it.
	if isHLSPlayback(query) {
		query.Set("protocol", "http")
	}
	query.Set("directPlay", "1")
	// Preserve a client's explicit Direct Stream capability. When the client
	// did not send it, keep the historical direct-play-only default. This
	// mirrors Plex's native decision flow: protocol=hls is only a preference,
	// while directPlay/directStream describe the client's actual intent.
	if strings.TrimSpace(query.Get("directStream")) == "" {
		query.Set("directStream", "0")
	}
	return query
}

func parseMetadataPath(path string) (string, bool) {
	const prefix = "/library/metadata/"
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(path, prefix)
	if strings.Contains(rest, "/") || !safePartID(rest) {
		return "", false
	}
	return path, true
}

func isPartFileRequest(path string) bool {
	_, ok := parsePartID(path)
	return ok
}

func parsePartID(path string) (string, bool) {
	const prefix = "/library/parts/"
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(path, prefix)
	segments := strings.Split(rest, "/")
	if (len(segments) != 2 && len(segments) != 3) || !safePartID(segments[0]) {
		return "", false
	}
	if len(segments) == 3 && !safePartID(segments[1]) {
		return "", false
	}
	fileSegment := segments[len(segments)-1]
	if fileSegment != "file" && !strings.HasPrefix(fileSegment, "file.") {
		return "", false
	}
	return segments[0], true
}

func decodeStructuredBody(contentEncoding string, body []byte, limit int64) ([]byte, error) {
	encoding := strings.ToLower(strings.TrimSpace(strings.Split(contentEncoding, ",")[0]))
	if encoding != "gzip" {
		return body, nil
	}
	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	decoded, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(decoded)) > limit {
		return nil, fmt.Errorf("decoded structured response exceeds limit")
	}
	return decoded, nil
}

func copyMediaRequestHeaders(destination, source http.Header) {
	for _, name := range []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since", "Accept", "User-Agent"} {
		for _, value := range source.Values(name) {
			destination.Add(name, value)
		}
	}
}

func copyMediaResponseHeaders(destination, source http.Header) {
	for _, name := range []string{"Accept-Ranges", "Cache-Control", "Content-Disposition", "Content-Encoding", "Content-Length", "Content-Range", "Content-Type", "ETag", "Last-Modified", "Location", "Retry-After"} {
		for _, value := range source.Values(name) {
			destination.Add(name, value)
		}
	}
}

func isStructuredContentType(contentType string) bool {
	contentType = strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	return contentType == "application/xml" || contentType == "text/xml" || contentType == "application/plex+xml" || contentType == "application/json" || contentType == "text/json"
}

func writeJSONError(writer http.ResponseWriter, status int, message string) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]string{"error": message})
}

func requestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(requestIDKey).(string)
	return value
}

func newRequestID() string {
	var data [12]byte
	if _, err := rand.Read(data[:]); err == nil {
		return hex.EncodeToString(data[:])
	}
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func resolvedTargetHost(rawURL string) string {
	target, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return target.Hostname()
}

func mediaContainerFromURL(rawURL string) string {
	target, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	extension := strings.TrimPrefix(strings.ToLower(path.Ext(target.Path)), ".")
	switch extension {
	case "mp4", "m4v", "mov":
		return "mp4"
	case "mkv", "mka":
		return "mkv"
	case "webm":
		return "webm"
	case "ts", "m2ts":
		return "mpegts"
	case "avi":
		return "avi"
	default:
		return ""
	}
}

type captureReadCloser struct {
	source   io.ReadCloser
	limit    int64
	buffer   bytes.Buffer
	overflow bool
	done     bool
	onDone   func([]byte)
}

func (reader *captureReadCloser) Read(buffer []byte) (int, error) {
	count, err := reader.source.Read(buffer)
	if count > 0 && !reader.overflow {
		if int64(reader.buffer.Len()+count) > reader.limit {
			reader.overflow = true
		} else {
			_, _ = reader.buffer.Write(buffer[:count])
		}
	}
	if err == io.EOF && !reader.done {
		reader.done = true
		if !reader.overflow && reader.onDone != nil {
			reader.onDone(reader.buffer.Bytes())
		}
	}
	return count, err
}

func (reader *captureReadCloser) Close() error {
	return reader.source.Close()
}

type loggingResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func newLoggingResponseWriter(writer http.ResponseWriter) *loggingResponseWriter {
	return &loggingResponseWriter{ResponseWriter: writer}
}

func (writer *loggingResponseWriter) WriteHeader(status int) {
	if writer.status != 0 {
		return
	}
	writer.status = status
	writer.ResponseWriter.WriteHeader(status)
}

func (writer *loggingResponseWriter) Write(data []byte) (int, error) {
	if writer.status == 0 {
		writer.WriteHeader(http.StatusOK)
	}
	count, err := writer.ResponseWriter.Write(data)
	writer.bytes += int64(count)
	return count, err
}

func (writer *loggingResponseWriter) Flush() {
	if writer.status == 0 {
		writer.WriteHeader(http.StatusOK)
	}
	if flusher, ok := writer.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (writer *loggingResponseWriter) ReadFrom(reader io.Reader) (int64, error) {
	if writer.status == 0 {
		writer.WriteHeader(http.StatusOK)
	}
	if from, ok := writer.ResponseWriter.(io.ReaderFrom); ok {
		count, err := from.ReadFrom(reader)
		writer.bytes += count
		return count, err
	}
	count, err := io.Copy(writer.ResponseWriter, reader)
	writer.bytes += count
	return count, err
}

func (writer *loggingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := writer.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijacking is not supported")
	}
	return hijacker.Hijack()
}

func (writer *loggingResponseWriter) Push(target string, options *http.PushOptions) error {
	pusher, ok := writer.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return pusher.Push(target, options)
}

func (writer *loggingResponseWriter) Unwrap() http.ResponseWriter {
	return writer.ResponseWriter
}
