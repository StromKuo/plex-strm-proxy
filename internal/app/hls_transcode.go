package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const hlsResourcePrefix = "/video/:/transcode/universal/session/"

const hlsSegmentDurationSeconds = 5.0
const hlsSeekWindowSeconds = 60.0
const hlsSeekWindowSegments = 12
const hlsSequentialLookaheadSegments = 2
const hlsResourceWaitSeconds = 90
const hlsSessionIdleTimeout = 3 * time.Minute

type hlsSession struct {
	id                string
	sourceURL         string
	directory         string
	width             int
	height            int
	bitrateKbps       int
	duration          float64
	startOffset       float64
	startSegment      int
	vod               bool
	started           bool
	done              bool
	processErr        error
	lastAccess        time.Time
	cmd               *exec.Cmd
	processDone       chan struct{}
	processStopReason hlsProcessStopReason
	copyAttempt       bool
	audioTranscode    bool
	sourceFile        string
	sourceReady       chan struct{}
	sourceErr         error
	sourceStarted     bool
	seekSegments      map[int]bool
	mu                sync.Mutex
}

type hlsProcessStopReason uint8

const (
	hlsStopNone hlsProcessStopReason = iota
	hlsStopSeek
	hlsStopSessionReplaced
	hlsStopExpired
)

type hlsTranscoder struct {
	root        string
	ffmpegPath  string
	ttl         time.Duration
	copyFirst   bool
	policy      TargetPolicy
	mediaClient *http.Client
	logger      *slog.Logger

	mu       sync.Mutex
	sessions map[string]*hlsSession
}

func newHLSTranscoder(root, ffmpegPath string, ttl time.Duration, copyFirst bool, policy TargetPolicy, mediaClient *http.Client, logger *slog.Logger) (*hlsTranscoder, error) {
	if strings.TrimSpace(root) == "" || strings.TrimSpace(ffmpegPath) == "" {
		return nil, fmt.Errorf("HLS transcoder root and ffmpeg path are required")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("HLS transcoder TTL must be positive")
	}
	if mediaClient == nil {
		return nil, fmt.Errorf("HLS transcoder media client is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create HLS transcode root: %w", err)
	}
	transcoder := &hlsTranscoder{
		root:        filepath.Clean(root),
		ffmpegPath:  ffmpegPath,
		ttl:         ttl,
		copyFirst:   copyFirst,
		policy:      policy,
		mediaClient: mediaClient,
		logger:      logger,
		sessions:    make(map[string]*hlsSession),
	}
	go transcoder.cleanupLoop()
	return transcoder, nil
}

func (t *hlsTranscoder) cleanupLoop() {
	interval := t.ttl / 2
	if interval <= 0 || interval > 30*time.Second {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		t.mu.Lock()
		t.cleanupExpiredLocked(time.Now())
		t.mu.Unlock()
	}
}

func (t *hlsTranscoder) register(rawID, sourceURL string, query url.Values, audioTranscode bool) (string, error) {
	id, err := normalizeHLSSessionID(rawID)
	if err != nil {
		return "", err
	}

	width, height := parseVideoResolution(query.Get("videoResolution"))
	bitrate := parseBitrateKbps(query.Get("maxVideoBitrate"))
	startOffset := parseHLSStartOffset(query)
	directory := filepath.Join(t.root, id)

	t.mu.Lock()
	t.cleanupExpiredLocked(time.Now())
	if existing, ok := t.sessions[id]; ok {
		existing.mu.Lock()
		if existing.started && !existing.done && existing.sourceURL == sourceURL {
			existing.lastAccess = time.Now()
			existing.mu.Unlock()
			t.mu.Unlock()
			return id, nil
		}
		if existing.cmd != nil && !existing.done {
			existing.processStopReason = hlsStopSessionReplaced
			_ = existing.cmd.Process.Kill()
		}
		existing.mu.Unlock()
		delete(t.sessions, id)
	}
	if err := os.RemoveAll(directory); err != nil {
		t.mu.Unlock()
		return "", fmt.Errorf("clear HLS transcode session: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(directory, "base"), 0o700); err != nil {
		t.mu.Unlock()
		return "", fmt.Errorf("create HLS transcode session: %w", err)
	}
	t.sessions[id] = &hlsSession{
		id:             id,
		sourceURL:      sourceURL,
		directory:      directory,
		width:          width,
		height:         height,
		bitrateKbps:    bitrate,
		startOffset:    startOffset,
		startSegment:   int(math.Floor(startOffset / hlsSegmentDurationSeconds)),
		audioTranscode: audioTranscode,
		lastAccess:     time.Now(),
		seekSegments:   make(map[int]bool),
	}
	t.mu.Unlock()
	return id, nil
}

func (t *hlsTranscoder) session(id string) (*hlsSession, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cleanupExpiredLocked(time.Now())
	session, ok := t.sessions[id]
	if !ok {
		return nil, false
	}
	session.mu.Lock()
	session.lastAccess = time.Now()
	session.mu.Unlock()
	return session, true
}

func (t *hlsTranscoder) start(ctx context.Context, id string) (string, error) {
	session, ok := t.session(id)
	if !ok {
		return "", fmt.Errorf("HLS transcode session %q was not found", id)
	}

	session.mu.Lock()
	if !session.started {
		validatedURL, validationErr := ResolveMediaTarget(ctx, t.mediaClient, t.policy, session.sourceURL)
		if validationErr != nil {
			session.mu.Unlock()
			return "", fmt.Errorf("HLS source is not allowed: %w", validationErr)
		}
		// Keep ffprobe and ffmpeg on the exact URL whose redirect chain was
		// checked by TargetPolicy before the subprocess is started.
		session.sourceURL = validatedURL
		if duration, err := t.probeDuration(ctx, session.sourceURL); err == nil && duration > 0 {
			session.duration = duration
			if session.startOffset >= duration {
				session.startOffset = math.Max(0, duration-5.0)
			}
			session.startSegment = int(math.Floor(session.startOffset / hlsSegmentDurationSeconds))
			session.vod = true
			if err := writeHLSVODPlaylist(filepath.Join(session.directory, "base", "index.m3u8"), duration); err != nil {
				session.mu.Unlock()
				return "", fmt.Errorf("write HLS VOD playlist: %w", err)
			}
			t.logger.Info("using known-duration HLS VOD playlist", "session", id, "duration_seconds", duration, "start_offset_seconds", session.startOffset, "start_segment", session.startSegment)
		} else if err != nil {
			t.logger.Warn("could not determine STRM media duration; using growing HLS playlist", "session", id, "error", err)
		}
		session.copyAttempt = t.copyFirst
		args := buildFFmpegArgs(session)
		session.cmd = exec.Command(t.ffmpegPath, args...)
		session.cmd.Stdout = io.Discard
		var stderr bytes.Buffer
		session.cmd.Stderr = &stderr
		if err := session.cmd.Start(); err != nil {
			session.mu.Unlock()
			return "", fmt.Errorf("start ffmpeg: %w", err)
		}
		session.processDone = make(chan struct{})
		session.started = true
		t.logger.Info("started STRM HLS playback", "session", id, "mode", hlsProcessMode(session))
		cmd := session.cmd
		processDone := session.processDone
		go t.waitForProcess(session, cmd, processDone, &stderr)
	}
	playlist := filepath.Join(session.directory, "base", "index.m3u8")
	vod := session.vod
	session.mu.Unlock()

	if !vod {
		if err := waitForHLSFile(ctx, playlist, session); err != nil {
			return "", err
		}
	}
	if _, err := os.Stat(playlist); err != nil {
		return "", err
	}
	return renderHLSMasterPlaylist(session), nil
}

func (t *hlsTranscoder) probeDuration(ctx context.Context, sourceURL string) (float64, error) {
	if _, err := t.policy.ValidateRawURL(ctx, sourceURL); err != nil {
		return 0, fmt.Errorf("ffprobe HLS source is not allowed: %w", err)
	}
	probeContext, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	probePath := filepath.Join(filepath.Dir(t.ffmpegPath), "ffprobe")
	command := exec.CommandContext(probeContext, probePath,
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		sourceURL,
	)
	output, err := command.Output()
	if err != nil {
		if probeContext.Err() != nil {
			return 0, probeContext.Err()
		}
		return 0, err
	}
	duration, err := strconv.ParseFloat(strings.TrimSpace(string(output)), 64)
	if err != nil || math.IsNaN(duration) || math.IsInf(duration, 0) || duration <= 0 {
		return 0, fmt.Errorf("invalid media duration %q", strings.TrimSpace(string(output)))
	}
	return duration, nil
}

func (t *hlsTranscoder) waitForProcess(session *hlsSession, cmd *exec.Cmd, processDone chan struct{}, stderr *bytes.Buffer) {
	err := cmd.Wait()
	stderrText := summarizeFFmpegStderr(stderr.String())
	session.mu.Lock()
	copyAttempt := session.copyAttempt
	stopReason := session.processStopReason
	session.processStopReason = hlsStopNone
	session.done = true
	session.processErr = err
	if shouldFallbackFromCopy(err, copyAttempt, stopReason) && t.hlsSourceAllowed(session) {
		session.copyAttempt = false
		session.done = false
		session.processErr = nil
		fallback := exec.Command(t.ffmpegPath, buildFFmpegArgs(session)...)
		fallback.Stdout = io.Discard
		var fallbackStderr bytes.Buffer
		fallback.Stderr = &fallbackStderr
		if startErr := fallback.Start(); startErr == nil {
			session.cmd = fallback
			session.processDone = make(chan struct{})
			fallbackDone := session.processDone
			session.mu.Unlock()
			close(processDone)
			t.logger.Warn("HLS stream copy failed; falling back to video transcode", "session", session.id, "error", err, "stderr", stderrText)
			go t.waitForProcess(session, fallback, fallbackDone, &fallbackStderr)
			return
		}
	}
	session.mu.Unlock()
	close(processDone)
	if err != nil {
		t.logger.Warn("STRM HLS transcode stopped with error", "session", session.id, "error", err, "stderr", stderrText)
	} else {
		t.logger.Info("STRM HLS transcode finished", "session", session.id)
	}
}

func (t *hlsTranscoder) hlsSourceAllowed(session *hlsSession) bool {
	if session == nil || session.sourceFile != "" {
		return session != nil
	}
	_, err := t.policy.ValidateRawURL(context.Background(), session.sourceURL)
	return err == nil
}

func shouldFallbackFromCopy(processErr error, copyAttempt bool, stopReason hlsProcessStopReason) bool {
	return processErr != nil && copyAttempt && stopReason == hlsStopNone
}

func (t *hlsTranscoder) filePath(id, relative string) (string, bool) {
	if !safeHLSSessionID(id) || relative == "" {
		return "", false
	}
	session, ok := t.session(id)
	if !ok {
		return "", false
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if filepath.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", false
	}
	full := filepath.Join(session.directory, clean)
	rel, err := filepath.Rel(session.directory, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return full, true
}

func (t *hlsTranscoder) ensureSourceCached(session *hlsSession) error {
	session.mu.Lock()
	if session.sourceFile != "" {
		path := session.sourceFile
		session.mu.Unlock()
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		session.mu.Lock()
		session.sourceFile = ""
	}
	if session.sourceReady == nil {
		session.sourceReady = make(chan struct{})
	}
	ready := session.sourceReady
	if !session.sourceStarted {
		session.sourceStarted = true
		go t.downloadSource(session, ready)
	}
	session.mu.Unlock()

	<-ready
	session.mu.Lock()
	err := session.sourceErr
	path := session.sourceFile
	session.mu.Unlock()
	if err != nil {
		return err
	}
	if path == "" {
		return fmt.Errorf("source cache completed without a file")
	}
	return nil
}

func (t *hlsTranscoder) downloadSource(session *hlsSession, ready chan struct{}) {
	defer close(ready)
	container := mediaContainerForURL(session.sourceURL)
	if container == "" {
		container = "media"
	}
	filename := filepath.Join(session.directory, "source."+container)
	temporary := filename + ".part"
	_ = os.Remove(temporary)

	target, err := url.Parse(session.sourceURL)
	if err == nil {
		err = t.policy.ValidateURL(context.Background(), target)
	}
	request, requestErr := http.NewRequest(http.MethodGet, session.sourceURL, nil)
	if err == nil {
		err = requestErr
	}
	if err == nil {
		request.Header.Set("User-Agent", "plex-strm-proxy/1.0")
	}
	var response *http.Response
	if err == nil {
		if t.mediaClient == nil {
			err = fmt.Errorf("media client is not configured")
		} else {
			// Use the same policy-constrained client as direct media requests.
			// This also validates every redirect, so a signed public URL cannot
			// redirect the seek cache into a private address.
			response, err = t.mediaClient.Do(request)
		}
	}
	if err == nil && (response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices) {
		if response != nil {
			_ = response.Body.Close()
		}
		err = fmt.Errorf("source download returned HTTP %d", response.StatusCode)
	}
	if err == nil {
		file, createErr := os.OpenFile(temporary, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if createErr != nil {
			_ = response.Body.Close()
			err = createErr
		} else {
			_, err = io.Copy(file, response.Body)
			closeErr := file.Close()
			_ = response.Body.Close()
			if err == nil {
				err = closeErr
			}
		}
	}
	if err == nil {
		err = os.Rename(temporary, filename)
	}
	session.mu.Lock()
	if err == nil {
		session.sourceFile = filename
	} else {
		_ = os.Remove(temporary)
		session.sourceErr = err
	}
	session.mu.Unlock()
	if err != nil {
		t.logger.Warn("failed to cache STRM source for HLS seek", "session", session.id, "target_host", resolvedTargetHost(session.sourceURL), "error", err)
	} else {
		t.logger.Info("cached STRM source for HLS seek", "session", session.id, "bytes", fileSize(filename))
	}
}

func fileSize(filename string) int64 {
	info, err := os.Stat(filename)
	if err != nil {
		return 0
	}
	return info.Size()
}

func (t *hlsTranscoder) ensureSegment(id string, segmentIndex int) {
	if segmentIndex < 0 {
		return
	}
	session, ok := t.session(id)
	if !ok {
		return
	}

	filename := filepath.Join(session.directory, "base", fmt.Sprintf("%05d.ts", segmentIndex))
	if _, err := os.Stat(filename); err == nil {
		return
	}
	// A VOD playlist contains the whole duration up front, while the main
	// ffmpeg process still creates segments in order. Do not mistake the next
	// normal segment request for a user seek just because it is beyond the
	// initial 60-second window. Only start a seek transcode when the requested
	// segment is clearly ahead of the segment currently being produced.
	latest := latestHLSFileIndex(filepath.Join(session.directory, "base"))
	if latest >= 0 && segmentIndex <= latest+hlsSequentialLookaheadSegments {
		return
	}

	session.mu.Lock()
	if !session.vod || segmentIndex == session.startSegment || (session.duration > 0 && float64(segmentIndex)*hlsSegmentDurationSeconds >= session.duration) || session.seekSegments[segmentIndex] {
		session.mu.Unlock()
		return
	}
	// A seek transcode covers its own 60-second window (twelve 5-second
	// segments). Do not start overlapping work for a segment already covered
	// by one of those windows, but do start a new window after the previous one
	// ends. The normal ffmpeg process may leave a gap while it is still working.
	for startSegment := range session.seekSegments {
		if segmentIndex >= startSegment && segmentIndex < startSegment+hlsSeekWindowSegments {
			session.mu.Unlock()
			return
		}
	}
	var mainProcessDone <-chan struct{}
	if session.cmd != nil && !session.done && segmentIndex >= session.startSegment+hlsSeekWindowSegments {
		// A far seek should not leave the normal from-zero ffmpeg process
		// reading the same signed source URL while another ffmpeg instance
		// opens it. Some OpenList/115 sources reject that second connection.
		session.processStopReason = hlsStopSeek
		_ = session.cmd.Process.Kill()
		mainProcessDone = session.processDone
	}
	session.seekSegments[segmentIndex] = true
	startOffset := float64(segmentIndex) * hlsSegmentDurationSeconds
	playlistName := fmt.Sprintf("seek-%05d.m3u8", segmentIndex)
	session.mu.Unlock()
	if mainProcessDone != nil {
		select {
		case <-mainProcessDone:
		case <-time.After(2 * time.Second):
			t.logger.Warn("timed out waiting for main HLS transcode to stop before seek", "session", id, "segment", segmentIndex)
		}
	}
	go t.startSeekTranscode(session, id, segmentIndex, startOffset, playlistName)
}

func (t *hlsTranscoder) startSeekTranscode(session *hlsSession, id string, segmentIndex int, startOffset float64, playlistName string) {
	if err := t.ensureSourceCached(session); err != nil {
		session.mu.Lock()
		delete(session.seekSegments, segmentIndex)
		session.mu.Unlock()
		t.logger.Warn("failed to cache source before HLS seek transcode", "session", id, "segment", segmentIndex, "error", err)
		return
	}
	args := buildFFmpegArgsForRange(session, startOffset, segmentIndex, playlistName, hlsSeekWindowSeconds)
	command := exec.Command(t.ffmpegPath, args...)
	command.Stdout = io.Discard
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		session.mu.Lock()
		delete(session.seekSegments, segmentIndex)
		session.mu.Unlock()
		t.logger.Warn("failed to start HLS seek transcode", "session", id, "segment", segmentIndex, "error", err)
		return
	}
	t.logger.Info("started HLS seek transcode", "session", id, "segment", segmentIndex, "source", "local-cache")
	go func() {
		err := command.Wait()
		session.mu.Lock()
		delete(session.seekSegments, segmentIndex)
		session.mu.Unlock()
		if err != nil {
			t.logger.Warn("HLS seek transcode stopped with error", "session", id, "segment", segmentIndex, "error", err, "stderr", summarizeFFmpegStderr(stderr.String()))
		}
	}()
}

var ffmpegURLPattern = regexp.MustCompile(`https?://[^\s]+`)

func summarizeFFmpegStderr(value string) string {
	value = strings.TrimSpace(ffmpegURLPattern.ReplaceAllString(value, "URL_MASKED"))
	if len(value) > 500 {
		return value[:500]
	}
	return value
}

func (t *hlsTranscoder) cleanupExpiredLocked(now time.Time) {
	idleTimeout := t.ttl
	if idleTimeout > hlsSessionIdleTimeout {
		idleTimeout = hlsSessionIdleTimeout
	}
	for id, session := range t.sessions {
		session.mu.Lock()
		idleExpired := now.Sub(session.lastAccess) > idleTimeout
		expired := now.Sub(session.lastAccess) > t.ttl
		active := session.started && !session.done
		if idleExpired && active && session.cmd != nil {
			session.processStopReason = hlsStopExpired
			_ = session.cmd.Process.Kill()
			t.logger.Info("stopping idle STRM HLS session", "session", id, "idle_seconds", int(now.Sub(session.lastAccess).Seconds()))
		}
		session.mu.Unlock()
		if (expired || idleExpired) && !active {
			_ = os.RemoveAll(session.directory)
			delete(t.sessions, id)
		}
	}
}

func waitForHLSFile(ctx context.Context, filename string, session *hlsSession) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(hlsResourceWaitSeconds * time.Second)
	defer deadline.Stop()
	for {
		if info, err := os.Stat(filename); err == nil && info.Size() > 0 {
			return nil
		}
		session.mu.Lock()
		done, processErr := session.done, session.processErr
		seekActive := len(session.seekSegments) > 0
		session.mu.Unlock()
		if done && !seekActive {
			if processErr == nil {
				return fmt.Errorf("ffmpeg finished before HLS playlist was created")
			}
			return fmt.Errorf("ffmpeg failed before HLS playlist was created: %w", processErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("timed out waiting for HLS playlist")
		case <-ticker.C:
		}
	}
}

func buildFFmpegArgs(session *hlsSession) []string {
	return buildFFmpegArgsForRange(session, session.startOffset, session.startSegment, hlsFFmpegPlaylistName(session), 0)
}

func buildFFmpegArgsForRange(session *hlsSession, startOffset float64, startSegment int, playlistName string, clipDuration float64) []string {
	videoBitrate := session.bitrateKbps - 128
	if videoBitrate < 256 {
		videoBitrate = 256
	}
	source := session.sourceURL
	if session.sourceFile != "" {
		source = session.sourceFile
	}
	localSource := source != session.sourceURL
	args := []string{"-hide_banner", "-loglevel", "error", "-y"}
	if startOffset > 0 && localSource {
		args = append(args, "-ss", strconv.FormatFloat(startOffset, 'f', 3, 64))
	} else if startOffset > 0 {
		// OpenList/115 sources may reject the byte range requested by ffmpeg's
		// input seek. Mark the connection as forward-only and put -ss after -i
		// so ffmpeg reads the response sequentially and seeks in decoded input.
		args = append(args, "-seekable", "0")
	}
	args = append(args, []string{
		"-i", source,
	}...)
	if startOffset > 0 && !localSource {
		args = append(args, "-ss", strconv.FormatFloat(startOffset, 'f', 3, 64))
	}
	if clipDuration > 0 {
		args = append(args, "-t", strconv.FormatFloat(clipDuration, 'f', 3, 64))
	}
	hlsArgs := []string{
		"-map", "0:v:0", "-map", "0:a:0?",
		"-f", "hls", "-hls_time", "5", "-hls_list_size", "0",
	}
	if !session.copyAttempt {
		hlsArgs = append(hlsArgs, "-hls_flags", "independent_segments")
	}
	hlsArgs = append(hlsArgs,
		"-start_number", strconv.Itoa(startSegment),
		"-hls_segment_filename", filepath.Join(session.directory, "base", "%05d.ts"),
		filepath.Join(session.directory, "base", playlistName),
	)
	args = append(args, hlsArgs...)
	codecArgs := []string{"-c:v", "libx264", "-preset", "veryfast", "-profile:v", "main", "-pix_fmt", "yuv420p", "-b:v", strconv.Itoa(videoBitrate) + "k", "-maxrate", strconv.Itoa(session.bitrateKbps) + "k", "-bufsize", strconv.Itoa(session.bitrateKbps*2) + "k", "-c:a", "aac", "-b:a", "128k", "-ac", "2", "-ar", "48000", "-g", "250", "-keyint_min", "1", "-sc_threshold", "0", "-force_key_frames", "expr:gte(t,n_forced*5)"}
	if session.copyAttempt {
		codecArgs = []string{"-c:v", "copy"}
		if session.audioTranscode {
			codecArgs = append(codecArgs, "-c:a", "aac", "-b:a", "128k", "-ac", "2", "-ar", "48000")
		} else {
			codecArgs = append(codecArgs, "-c:a", "copy")
		}
	}
	insertAt := len(args)
	for index, argument := range args {
		if argument == "-f" {
			insertAt = index
			break
		}
	}
	args = append(args[:insertAt], append(codecArgs, args[insertAt:]...)...)
	if !session.copyAttempt && session.width > 0 && session.height > 0 {
		filter := fmt.Sprintf("scale=w=%d:h=%d:force_original_aspect_ratio=decrease:force_divisible_by=2", session.width, session.height)
		filterAt := len(args)
		for index, argument := range args {
			if argument == "-c:v" {
				filterAt = index
				break
			}
		}
		args = append(args[:filterAt], append([]string{"-vf", filter}, args[filterAt:]...)...)
	}
	return args
}

func hlsProcessMode(session *hlsSession) string {
	if session != nil && session.copyAttempt {
		if session.audioTranscode {
			return "video-copy-audio-transcode"
		}
		return "stream-copy"
	}
	return "video-transcode"
}

func parseHLSStartOffset(query url.Values) float64 {
	value := strings.TrimSpace(query.Get("offset"))
	if value == "" {
		return 0
	}
	milliseconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || milliseconds <= 0 {
		return 0
	}
	return float64(milliseconds) / 1000.0
}

func hlsFFmpegPlaylistName(session *hlsSession) string {
	if session != nil && session.vod {
		return "ffmpeg.m3u8"
	}
	return "index.m3u8"
}

func writeHLSVODPlaylist(filename string, duration float64) error {
	segmentCount := int(math.Ceil(duration / hlsSegmentDurationSeconds))
	if segmentCount < 1 {
		return fmt.Errorf("media duration is too short: %f", duration)
	}

	var playlist strings.Builder
	playlist.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:5\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n")
	for index := 0; index < segmentCount; index++ {
		remaining := duration - float64(index)*hlsSegmentDurationSeconds
		currentDuration := math.Min(hlsSegmentDurationSeconds, remaining)
		if currentDuration <= 0 {
			break
		}
		fmt.Fprintf(&playlist, "#EXTINF:%.3f,\n%05d.ts\n", currentDuration, index)
	}
	playlist.WriteString("#EXT-X-ENDLIST\n")
	return os.WriteFile(filename, []byte(playlist.String()), 0o600)
}

func parseHLSSegmentIndex(relative string) (int, bool) {
	name := filepath.Base(filepath.FromSlash(relative))
	if !strings.HasSuffix(strings.ToLower(name), ".ts") {
		return 0, false
	}
	index, err := strconv.Atoi(strings.TrimSuffix(strings.ToLower(name), ".ts"))
	if err != nil || index < 0 {
		return 0, false
	}
	return index, true
}

func latestHLSFileIndex(directory string) int {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return -1
	}
	latest := -1
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		index, ok := parseHLSSegmentIndex(entry.Name())
		if ok && index > latest {
			latest = index
		}
	}
	return latest
}

func renderHLSMasterPlaylist(session *hlsSession) string {
	width, height := session.width, session.height
	if width <= 0 || height <= 0 {
		width, height = 1280, 720
	}
	return fmt.Sprintf("#EXTM3U\n#EXT-X-STREAM-INF:PROGRAM-ID=1,BANDWIDTH=%d,RESOLUTION=%dx%d\n%s/base/index.m3u8\n", session.bitrateKbps*1000, width, height, hlsResourcePrefix+session.id)
}

func parseVideoResolution(value string) (int, int) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(value)), "x")
	if len(parts) != 2 {
		return 0, 0
	}
	width, widthErr := strconv.Atoi(parts[0])
	height, heightErr := strconv.Atoi(parts[1])
	if widthErr != nil || heightErr != nil || width < 2 || height < 2 || width > 8192 || height > 8192 {
		return 0, 0
	}
	return width, height
}

func parseBitrateKbps(value string) int {
	bitrate, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || bitrate < 256 {
		return 2000
	}
	if bitrate > 100000 {
		return 100000
	}
	return bitrate
}

func normalizeHLSSessionID(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", fmt.Errorf("generate HLS session id: %w", err)
		}
		return hex.EncodeToString(random[:]), nil
	}
	if safeHLSSessionID(raw) {
		return raw, nil
	}
	return "", fmt.Errorf("invalid Plex HLS session id")
}

func safeHLSSessionID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '-' && char != '_' {
			return false
		}
	}
	return true
}

func parseHLSResourcePath(requestPath string) (string, string, bool) {
	if !strings.HasPrefix(requestPath, hlsResourcePrefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(requestPath, hlsResourcePrefix)
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || !safeHLSSessionID(parts[0]) || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func isHLSResourceRequest(requestPath string) bool {
	return strings.HasPrefix(requestPath, hlsResourcePrefix)
}

func hlsSessionIDFromRequest(request *http.Request) string {
	if request == nil {
		return ""
	}
	for _, value := range []string{
		request.URL.Query().Get("session"),
		request.URL.Query().Get("X-Plex-Session-Identifier"),
		request.Header.Get("X-Plex-Session-Identifier"),
	} {
		if strings.TrimSpace(value) == "" {
			continue
		}
		id, err := normalizeHLSSessionID(value)
		if err == nil {
			return id
		}
	}
	return ""
}
