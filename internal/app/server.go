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
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"
)

type contextKey string

const (
	requestIDKey   contextKey = "request-id"
	defaultSTRMKey contextKey = "default-strm-path"
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
	directByPart    map[string]directDecision
	probeMu         sync.Mutex
	probes          map[string]mediaProbeCacheEntry
}

type directDecision struct {
	mapping   PartMapping
	expiresAt time.Time
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
		directByPart:    make(map[string]directDecision),
		probes:          make(map[string]mediaProbeCacheEntry),
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
		s.handleHLSResource(tracked, request)
	case isPartFileRequest(request.URL.Path):
		s.handlePartFile(tracked, request)
	default:
		s.proxyToPlex(tracked, request, "")
	}

	s.logger.Info("request completed", "request_id", requestID, "method", request.Method, "path", request.URL.Path, "status", tracked.status, "bytes", tracked.bytes, "duration_ms", time.Since(started).Milliseconds())
}

func (s *Server) handleDecision(writer http.ResponseWriter, request *http.Request) {
	s.logger.Info("Plex decision client", "request_id", requestID(request.Context()), "product", request.Header.Get("X-Plex-Product"), "platform", request.Header.Get("X-Plex-Platform"), "model", request.Header.Get("X-Plex-Model"), "device", request.Header.Get("X-Plex-Device"), "user_agent", request.UserAgent(), "accept", request.Header.Get("Accept"), "path_kind", playbackPathKind(request.URL.Query().Get("path")), "protocol", request.URL.Query().Get("protocol"), "direct_play", request.URL.Query().Get("directPlay"), "direct_stream", request.URL.Query().Get("directStream"), "location", request.URL.Query().Get("location"), "session", hlsSessionIDFromRequest(request))
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
			plan, probe := s.selectPlaybackPlan(request, mapping, PlaybackStageMetadata, PlexDecisionUnknown, false)
			if plan.Plan == PlaybackPlanPlexTranscode && plan.Reason == PlaybackReasonProxyHLSDisabled {
				writeJSONError(writer, http.StatusNotImplemented, "STRM HLS fallback is disabled")
				return
			}
			if plan.Plan == PlaybackPlanProxyHLSAudioFallback {
				audioCodec := ""
				if probe != nil {
					audioCodec = probe.AudioCodec
				}
				s.logger.Info("using STRM HLS fallback for client audio capability", "request_id", requestID(request.Context()), "part_id", mapping.PartID, "audio_codec", audioCodec, "plan", plan.Plan, "reason", plan.Reason)
				if s.hls == nil {
					writeJSONError(writer, http.StatusNotImplemented, "STRM HLS fallback is disabled")
					return
				}
				sessionID, registerErr := s.registerHLSTranscode(request, mapping)
				if registerErr != nil {
					s.logger.Warn("failed to register STRM HLS fallback", "request_id", requestID(request.Context()), "part_id", mapping.PartID, "error", registerErr)
					writeJSONError(writer, http.StatusBadGateway, "unable to start STRM HLS fallback")
					return
				}
				s.logger.Info("returning synthetic STRM HLS decision", "request_id", requestID(request.Context()), "part_id", mapping.PartID, "session", sessionID, "metadata_content_type", lookup.ContentType, "metadata_bytes", len(lookup.Body))
				writeHLSTranscodeDecision(writer, request, mapping, lookup.ContentType, lookup.Body, sessionID)
				return
			}
			s.logger.Info("STRM decision client", "request_id", requestID(request.Context()), "product", request.Header.Get("X-Plex-Product"), "platform", request.Header.Get("X-Plex-Platform"), "model", request.Header.Get("X-Plex-Model"), "device", request.Header.Get("X-Plex-Device"), "user_agent", request.UserAgent(), "session", hlsSessionIDFromRequest(request), "protocol", request.URL.Query().Get("protocol"), "direct_play", request.URL.Query().Get("directPlay"), "direct_stream", request.URL.Query().Get("directStream"), "location", request.URL.Query().Get("location"))
			// Keep the Plex metadata path here. Plex treats a URL passed as
			// `path` as a media description endpoint and may try to parse the
			// remote video response as Plex metadata, which produces a 400 for
			// OpenList URLs. The resolved URL is used later by the Part handler,
			// where it can be redirected directly to the client.
			//
			// Do not force HLS merely because a client advertises HLS. If Plex
			// decides that direct play is possible, the Part request redirects to
			// the source. If Plex decides to transcode, the subsequent start
			// request is handled by the HLS fallback when enabled.
			query := prepareSTRMDirectPlaybackQuery(request, "")
			s.logger.Info("requesting direct-first STRM decision", "request_id", requestID(request.Context()), "metadata_path", metadataPath, "part_id", mapping.PartID, "target_host", resolvedTargetHost(mapping.ResolvedURL))
			clonedURL := *request.URL
			clonedURL.RawQuery = query.Encode()
			decisionRequest := request.Clone(request.Context())
			decisionRequest.URL = &clonedURL
			s.proxyToPlex(writer, decisionRequest, mapping.STRMPath)
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

	if isHLSStartRequest(request) {
		// Android may ask for an HLS start URL even after the preceding
		// decision response was rewritten to directplay (observed with
		// directPlay=0&directStream=1). Once that decision is known, the
		// start request is the last opportunity to keep the media path direct.
		// Redirect regardless of the request's stale capability flags; fall
		// back to HLS only when no direct decision was established.
		if directMapping, ok := s.directDecisionForStart(sessionID, mapping.PartID); ok {
			plan, _ := s.selectPlaybackPlan(request, directMapping, PlaybackStageTranscodeStart, PlexDecisionDirectPlay, true)
			s.logger.Info("redirecting HLS start to direct STRM source", "request_id", requestID(request.Context()), "part_id", directMapping.PartID, "target_host", resolvedTargetHost(directMapping.ResolvedURL), "plan", plan.Plan, "reason", plan.Reason)
			s.serveResolvedMedia(writer, request, directMapping)
			return
		}
		plan, _ := s.selectPlaybackPlan(request, mapping, PlaybackStageTranscodeStart, PlexDecisionTranscode, false)
		if plan.Plan == PlaybackPlanSTRMRedirect {
			s.logger.Info("redirecting explicit direct-play HLS start to STRM source", "request_id", requestID(request.Context()), "part_id", mapping.PartID, "target_host", resolvedTargetHost(mapping.ResolvedURL), "plan", plan.Plan, "reason", plan.Reason)
			s.serveResolvedMedia(writer, request, mapping)
			return
		}
		if plan.Plan == PlaybackPlanPlexTranscode {
			s.proxyToPlex(writer, request, "")
			return
		}
		if _, ok := s.hls.session(sessionID); ok {
			s.logger.Info("starting STRM HLS transcode", "request_id", requestID(request.Context()), "metadata_path", metadataPath, "part_id", mapping.PartID, "session", sessionID, "plan", plan.Plan, "reason", plan.Reason)
			s.handleHLSTranscodeStart(writer, request, sessionID)
			return
		}
		registeredID, registerErr := s.registerHLSTranscode(request, mapping)
		if registerErr != nil {
			s.logger.Warn("failed to register STRM HLS transcode at start", "request_id", requestID(request.Context()), "part_id", mapping.PartID, "error", registerErr)
			writeJSONError(writer, http.StatusBadGateway, "unable to start STRM HLS transcode")
			return
		}
		s.handleHLSTranscodeStart(writer, request, registeredID)
		return
	}
	query := prepareSTRMPlaybackQuery(request)
	query.Set("path", mapping.ResolvedURL)
	s.logger.Info("forwarding STRM transcode start to Plex", "request_id", requestID(request.Context()), "metadata_path", metadataPath, "part_id", mapping.PartID, "target_host", resolvedTargetHost(mapping.ResolvedURL))
	clonedURL := *request.URL
	clonedURL.RawQuery = query.Encode()
	startRequest := request.Clone(request.Context())
	startRequest.URL = &clonedURL
	s.proxyToPlex(writer, startRequest, mapping.STRMPath)
}

func (s *Server) registerHLSTranscode(request *http.Request, mapping PartMapping) (string, error) {
	if s.hls == nil {
		return "", fmt.Errorf("HLS transcoder is disabled")
	}
	plan, _ := s.selectPlaybackPlan(request, mapping, PlaybackStageTranscodeStart, PlexDecisionTranscode, false)
	audioTranscode := plan.Plan == PlaybackPlanProxyHLSAudioFallback
	validatedURL, err := ResolveMediaTarget(request.Context(), s.mediaClient, s.policy, mapping.ResolvedURL)
	if err != nil {
		return "", fmt.Errorf("validate STRM HLS source: %w", err)
	}
	return s.hls.register(hlsSessionIDFromRequest(request), validatedURL, request.URL.Query(), audioTranscode)
}

func (s *Server) handleHLSTranscodeStart(writer http.ResponseWriter, request *http.Request, sessionID string) {
	playlist, err := s.hls.start(request.Context(), sessionID)
	if err != nil {
		s.logger.Warn("failed to start STRM HLS transcode", "request_id", requestID(request.Context()), "session", sessionID, "error", err)
		writeJSONError(writer, http.StatusBadGateway, "STRM HLS transcode unavailable")
		return
	}
	setHLSResponseHeaders(writer, request, "application/vnd.apple.mpegurl")
	writer.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(writer, playlist)
}

func (s *Server) handleHLSResource(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writeJSONError(writer, http.StatusMethodNotAllowed, "HLS resource supports GET and HEAD only")
		return
	}
	sessionID, relative, ok := parseHLSResourcePath(request.URL.Path)
	if !ok || s.hls == nil {
		s.proxyToPlex(writer, request, "")
		return
	}
	filename, ok := s.hls.filePath(sessionID, relative)
	if !ok {
		s.proxyToPlex(writer, request, "")
		return
	}
	if _, err := os.Stat(filename); err != nil {
		session, sessionOK := s.hls.session(sessionID)
		if !sessionOK {
			s.proxyToPlex(writer, request, "")
			return
		}
		if strings.HasSuffix(strings.ToLower(relative), ".m3u8") {
			if _, err := s.hls.start(request.Context(), sessionID); err != nil {
				s.logger.Warn("failed to start STRM HLS transcode from playlist request", "request_id", requestID(request.Context()), "session", sessionID, "error", err)
				writeJSONError(writer, http.StatusBadGateway, "STRM HLS transcode unavailable")
				return
			}
		}
		if segmentIndex, segmentOK := parseHLSSegmentIndex(relative); segmentOK {
			s.hls.ensureSegment(sessionID, segmentIndex)
		}
		if err := waitForHLSFile(request.Context(), filename, session); err != nil {
			writeJSONError(writer, http.StatusNotFound, "HLS resource is not ready")
			return
		}
	}
	contentType := "application/octet-stream"
	if strings.HasSuffix(strings.ToLower(filename), ".m3u8") {
		contentType = "application/vnd.apple.mpegurl"
	} else if strings.HasSuffix(strings.ToLower(filename), ".ts") {
		contentType = "video/mp2t"
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
	writer.Header().Set("X-Plex-Strm-Proxy", "hls-transcode")
	setMediaCORSHeaders(writer, request)
	if isJSONContentType(metadataContentType) {
		if rewritten, changed, err := renderHLSTranscodeDecisionJSON(metadataBody, mapping, query, sessionID); err == nil && changed {
			writer.Header().Set("Content-Type", "application/json; charset=utf-8")
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write(rewritten)
			return
		}
	}
	writer.Header().Set("Content-Type", "application/xml; charset=utf-8")
	if rewritten, changed, err := renderHLSTranscodeDecisionXML(metadataBody, metadataContentType, mapping, query, sessionID); err == nil && changed {
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(rewritten)
		return
	}
	_, _ = fmt.Fprintf(writer, `<?xml version="1.0" encoding="UTF-8"?>
<MediaContainer size="1" allowSync="1" directPlayDecisionCode="3000" directPlayDecisionText="App cannot direct play this item. Direct play is disabled." generalDecisionCode="1001" generalDecisionText="Direct play not available; Conversion OK." identifier="com.plexapp.plugins.library" resourceSession="%s" transcodeDecisionCode="1001" transcodeDecisionText="Direct play not available; Conversion OK.">
<Video key="%s" type="movie"><Media id="%s" bitrate="%d" container="mpegts" protocol="hls" videoCodec="h264" audioCodec="aac" width="%d" height="%d" selected="1"><Part id="%s" key="%s" container="mpegts" protocol="hls" decision="transcode" width="%d" height="%d" selected="1"><Stream streamType="1" codec="h264" width="%d" height="%d" decision="transcode" location="segments-av" /><Stream streamType="2" codec="aac" channels="2" decision="transcode" location="segments-av" /></Part></Media></Video>
</MediaContainer>`, sessionID, metadataPath, mapping.PartID, bitrate, width, height, mapping.PartID, hlsPartKey(sessionID), width, height, width, height)
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
		s.ingestPlexStructuredResponse(responseRequestID, contentType, decoded, defaultSTRMPath)
		upstreamSummary, upstreamSummaryOK := summarizeDecisionResponse(contentType, decoded)
		s.logger.Info("STRM decision response captured", "request_id", responseRequestID, "phase", "upstream", "content_type", contentType, "decoded_bytes", len(decoded), "summary_ok", upstreamSummaryOK, "summary", upstreamSummary)
		responseBody := rawBody
		if mapping, ok := decisionResponsePartMapping(s.mappings, contentType, decoded); ok {
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
		// Plex Web uses the declared container to choose the playback branch.
		// Keep the STRM compatibility declaration that selects direct play; the
		// media handler below corrects the actual Content-Type from the bytes.
		rewritten, changed, err := s.rewriteSTRMMetadataResponse(response.Request, contentType, decoded, isDetailedMetadataResponse(response.Request))
		if err != nil {
			return fmt.Errorf("rewrite Plex STRM metadata: %w", err)
		}
		s.logger.Info("Plex metadata response inspected", "request_id", responseRequestID, "path", response.Request.URL.Path, "accept", response.Request.Header.Get("Accept"), "content_type", contentType, "content_encoding", contentEncoding, "decoded_bytes", len(decoded), "rewritten_bytes", len(rewritten), "changed", changed, "probe_media", isDetailedMetadataResponse(response.Request))
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
		return mapping.ResolvedURL
	}
	var probeFor metadataProbeFunc
	if probeMedia {
		probeFor = func(part partRecord) *mediaProbe {
			if !directAllowed(part) {
				return nil
			}
			mapping, ok := s.mappings.Get(part.ID)
			if !ok || mapping.Kind != PartKindSTRM || mapping.ResolvedURL == "" {
				return nil
			}
			probe, ok := s.probeSTRMMedia(ctx, mapping.ResolvedURL)
			if !ok {
				return nil
			}
			return &probe
		}
	}
	return rewriteSTRMMetadataWithContainerAndFileAndProbe(contentType, body, containerFor, fileFor, probeFor)
}

func isDetailedMetadataResponse(request *http.Request) bool {
	if request == nil || request.URL == nil {
		return false
	}
	_, ok := parseMetadataPath(request.URL.Path)
	return ok
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

func (s *Server) rememberDirectDecision(request *http.Request, contentType string, body []byte) {
	if request == nil || !isDecisionRequest(request) || !decisionBodyIsDirect(contentType, body) {
		return
	}
	sessionID := hlsSessionIDFromRequest(request)
	if sessionID == "" {
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
			s.directDecisions[sessionID] = directDecision{mapping: mapping, expiresAt: time.Now().Add(s.cfg.MappingCacheTTL)}
			s.directByPart[mapping.PartID] = directDecision{mapping: mapping, expiresAt: time.Now().Add(s.cfg.MappingCacheTTL)}
			s.decisionMu.Unlock()
			s.logger.Info("remembered direct STRM decision", "request_id", requestID(request.Context()), "session", sessionID, "part_id", mapping.PartID, "target_host", resolvedTargetHost(mapping.ResolvedURL))
			return
		}
	}
}

func (s *Server) directDecisionForStart(sessionID, partID string) (PartMapping, bool) {
	if mapping, ok := s.directDecision(sessionID); ok {
		return mapping, true
	}
	if partID == "" {
		return PartMapping{}, false
	}
	s.decisionMu.Lock()
	defer s.decisionMu.Unlock()
	decision, ok := s.directByPart[partID]
	if !ok {
		return PartMapping{}, false
	}
	if time.Now().After(decision.expiresAt) {
		delete(s.directByPart, partID)
		return PartMapping{}, false
	}
	return decision.mapping, true
}

func (s *Server) directDecision(sessionID string) (PartMapping, bool) {
	if sessionID == "" {
		return PartMapping{}, false
	}
	s.decisionMu.Lock()
	defer s.decisionMu.Unlock()
	decision, ok := s.directDecisions[sessionID]
	if !ok {
		return PartMapping{}, false
	}
	if time.Now().After(decision.expiresAt) {
		delete(s.directDecisions, sessionID)
		return PartMapping{}, false
	}
	return decision.mapping, true
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
	query.Set("directStream", "0")
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
