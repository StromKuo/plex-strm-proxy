package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type mediaProbe struct {
	Duration            float64
	BitrateKbps         int
	Width               int
	Height              int
	VideoCodec          string
	AudioCodec          string
	AudioChannels       int
	VideoBitrateKbps    int
	AudioBitrateKbps    int
	VideoFrameRate      float64
	VideoProfile        string
	AudioProfile        string
	VideoBitDepth       int
	VideoCodedWidth     int
	VideoCodedHeight    int
	VideoScanType       string
	AudioSampleRate     int
	AudioChannelLayout  string
	PreferredAudioIndex int
	ColorRange          string
	ColorSpace          string
	ColorTransfer       string
	ColorPrimaries      string
	Container           string
	SizeBytes           int64
	Streams             []mediaProbeStream
}

type mediaProbeStream struct {
	StreamType     int
	Index          int
	Codec          string
	BitrateKbps    int
	Width          int
	Height         int
	CodedWidth     int
	CodedHeight    int
	Channels       int
	SampleRate     int
	ChannelLayout  string
	Profile        string
	BitDepth       int
	FrameRate      float64
	ScanType       string
	Language       string
	Title          string
	Default        bool
	Forced         bool
	ColorRange     string
	ColorSpace     string
	ColorTransfer  string
	ColorPrimaries string
}

type mediaProbeCacheEntry struct {
	value     mediaProbe
	expiresAt time.Time
}

type ffprobeResult struct {
	Streams []struct {
		Index              int         `json:"index"`
		CodecType          string      `json:"codec_type"`
		CodecName          string      `json:"codec_name"`
		BitRate            string      `json:"bit_rate"`
		Width              int         `json:"width"`
		Height             int         `json:"height"`
		CodedWidth         int         `json:"coded_width"`
		CodedHeight        int         `json:"coded_height"`
		Channels           int         `json:"channels"`
		SampleRate         string      `json:"sample_rate"`
		ChannelLayout      string      `json:"channel_layout"`
		Profile            string      `json:"profile"`
		PixFmt             string      `json:"pix_fmt"`
		BitsPerRawSample   flexibleInt `json:"bits_per_raw_sample"`
		BitsPerCodedSample flexibleInt `json:"bits_per_coded_sample"`
		FrameRate          string      `json:"r_frame_rate"`
		FieldOrder         string      `json:"field_order"`
		ColorRange         string      `json:"color_range"`
		ColorSpace         string      `json:"color_space"`
		ColorTransfer      string      `json:"color_transfer"`
		ColorPrimaries     string      `json:"color_primaries"`
		Level              int         `json:"level"`
		Refs               int         `json:"refs"`
		Disposition        struct {
			Default     int `json:"default"`
			Forced      int `json:"forced"`
			AttachedPic int `json:"attached_pic"`
		} `json:"disposition"`
		Tags struct {
			Language string `json:"language"`
			Title    string `json:"title"`
		} `json:"tags"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
		BitRate  string `json:"bit_rate"`
		Size     string `json:"size"`
		Name     string `json:"format_name"`
	} `json:"format"`
}

// ffprobe emits some numeric fields as either JSON numbers or strings,
// depending on the demuxer. Accept both representations so one unusual
// stream does not disable metadata probing for the whole media item.
type flexibleInt int

func (value *flexibleInt) UnmarshalJSON(data []byte) error {
	if strings.TrimSpace(string(data)) == "null" {
		*value = 0
		return nil
	}
	var number int
	if err := json.Unmarshal(data, &number); err == nil {
		*value = flexibleInt(number)
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		*value = 0
		return nil
	}
	number, err := strconv.Atoi(text)
	if err != nil {
		return err
	}
	*value = flexibleInt(number)
	return nil
}

// probeMedia runs ffprobe against sourceURL. The caller must have validated
// the URL and its redirect chain via ResolveMediaTarget first; ffprobe then
// runs against the already-validated final URL without re-resolving.
func probeMedia(ctx context.Context, ffmpegPath, sourceURL string) (mediaProbe, error) {
	if strings.TrimSpace(ffmpegPath) == "" || strings.TrimSpace(sourceURL) == "" {
		return mediaProbe{}, fmt.Errorf("ffmpeg path and source URL are required")
	}
	probeContext, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	probePath := filepath.Join(filepath.Dir(ffmpegPath), "ffprobe")
	command := exec.CommandContext(probeContext, probePath,
		"-v", "error",
		"-probesize", "128000",
		"-analyzeduration", "500000",
		"-rw_timeout", "30000000",
		"-show_streams",
		"-show_format",
		"-of", "json",
		sourceURL,
	)
	output, err := command.Output()
	if err != nil {
		if probeContext.Err() != nil {
			return mediaProbe{}, probeContext.Err()
		}
		return mediaProbe{}, err
	}
	var result ffprobeResult
	if err := json.Unmarshal(output, &result); err != nil {
		return mediaProbe{}, fmt.Errorf("decode ffprobe output: %w", err)
	}
	probe := mediaProbe{PreferredAudioIndex: -1}
	if value, err := strconv.ParseFloat(strings.TrimSpace(result.Format.Duration), 64); err == nil && value > 0 {
		probe.Duration = value
	}
	if value, err := strconv.ParseInt(strings.TrimSpace(result.Format.BitRate), 10, 64); err == nil && value > 0 {
		probe.BitrateKbps = int(value / 1000)
	}
	if value := parseInt64(result.Format.Size); value > 0 {
		probe.SizeBytes = value
	}
	if result.Format.Name != "" {
		probe.Container = normalizeContainer(result.Format.Name)
	}
	for _, stream := range result.Streams {
		streamType := probeStreamType(stream.CodecType)
		if streamType == 0 || (streamType == 1 && stream.Disposition.AttachedPic != 0) {
			continue
		}
		parsedStream := mediaProbeStream{
			StreamType:     streamType,
			Index:          stream.Index,
			Codec:          plexCodecName(stream.CodecName),
			BitrateKbps:    parseKbps(stream.BitRate),
			Width:          stream.Width,
			Height:         stream.Height,
			CodedWidth:     stream.CodedWidth,
			CodedHeight:    stream.CodedHeight,
			Channels:       stream.Channels,
			SampleRate:     parseInt(stream.SampleRate),
			ChannelLayout:  strings.TrimSpace(stream.ChannelLayout),
			Profile:        normalizeStreamProfile(stream.CodecType, stream.Profile, int(stream.BitsPerRawSample), int(stream.BitsPerCodedSample)),
			BitDepth:       int(stream.BitsPerRawSample),
			FrameRate:      parseFrameRate(stream.FrameRate),
			ScanType:       scanType(stream.FieldOrder),
			Language:       plexLanguage(stream.Tags.Language),
			Title:          strings.TrimSpace(stream.Tags.Title),
			Default:        stream.Disposition.Default != 0,
			Forced:         stream.Disposition.Forced != 0,
			ColorRange:     strings.TrimSpace(stream.ColorRange),
			ColorSpace:     strings.TrimSpace(stream.ColorSpace),
			ColorTransfer:  strings.TrimSpace(stream.ColorTransfer),
			ColorPrimaries: strings.TrimSpace(stream.ColorPrimaries),
		}
		if parsedStream.BitDepth == 0 {
			parsedStream.BitDepth = int(stream.BitsPerCodedSample)
		}
		if parsedStream.Codec != "" {
			probe.Streams = append(probe.Streams, parsedStream)
		}
		switch strings.ToLower(stream.CodecType) {
		case "video":
			if probe.VideoCodec == "" {
				probe.VideoCodec = parsedStream.Codec
			}
			if probe.Width == 0 {
				probe.Width = stream.Width
			}
			if probe.Height == 0 {
				probe.Height = stream.Height
			}
			if probe.VideoBitrateKbps == 0 {
				probe.VideoBitrateKbps = parseKbps(stream.BitRate)
			}
			if probe.VideoProfile == "" {
				probe.VideoProfile = parsedStream.Profile
			}
			if probe.VideoCodedWidth == 0 {
				probe.VideoCodedWidth = stream.CodedWidth
			}
			if probe.VideoCodedHeight == 0 {
				probe.VideoCodedHeight = stream.CodedHeight
			}
			if probe.VideoBitDepth == 0 {
				probe.VideoBitDepth = int(stream.BitsPerRawSample)
				if probe.VideoBitDepth == 0 {
					probe.VideoBitDepth = int(stream.BitsPerCodedSample)
				}
			}
			if probe.VideoFrameRate == 0 {
				probe.VideoFrameRate = parseFrameRate(stream.FrameRate)
			}
			if probe.VideoScanType == "" {
				probe.VideoScanType = scanType(stream.FieldOrder)
			}
			if probe.ColorRange == "" {
				probe.ColorRange = strings.TrimSpace(stream.ColorRange)
			}
			if probe.ColorSpace == "" {
				probe.ColorSpace = strings.TrimSpace(stream.ColorSpace)
			}
			if probe.ColorTransfer == "" {
				probe.ColorTransfer = strings.TrimSpace(stream.ColorTransfer)
			}
			if probe.ColorPrimaries == "" {
				probe.ColorPrimaries = strings.TrimSpace(stream.ColorPrimaries)
			}
		case "audio":
			if probe.AudioCodec == "" {
				probe.AudioCodec = parsedStream.Codec
			}
			if probe.AudioChannels == 0 {
				probe.AudioChannels = stream.Channels
			}
			if probe.AudioBitrateKbps == 0 {
				probe.AudioBitrateKbps = parseKbps(stream.BitRate)
			}
			if probe.AudioProfile == "" {
				probe.AudioProfile = parsedStream.Profile
			}
			if probe.AudioSampleRate == 0 {
				probe.AudioSampleRate = parseInt(stream.SampleRate)
			}
			if probe.AudioChannelLayout == "" {
				probe.AudioChannelLayout = strings.TrimSpace(stream.ChannelLayout)
			}
		}
	}
	selectPreferredAudio(&probe)
	if probe.VideoCodec == "" && probe.AudioCodec == "" && probe.Duration <= 0 {
		return mediaProbe{}, fmt.Errorf("ffprobe returned no usable media information")
	}
	return probe, nil
}

func selectPreferredAudio(probe *mediaProbe) {
	if probe == nil {
		return
	}
	firstAudio := -1
	defaultAudio := -1
	for _, stream := range probe.Streams {
		if stream.StreamType != 2 {
			continue
		}
		if firstAudio < 0 {
			firstAudio = stream.Index
		}
		if defaultAudio < 0 && stream.Default {
			defaultAudio = stream.Index
		}
	}
	if firstAudio < 0 {
		probe.PreferredAudioIndex = -1
		return
	}

	// AAC is broadly hardware-decoded by Android and is preferable when the
	// source contains both AAC and a less portable multichannel codec such as
	// E-AC-3. Keep the default track's language when possible; otherwise use
	// the first AAC track and finally fall back to the source's default/first
	// audio stream. This only selects the advertised track; it never changes
	// the source bytes, and HLS remains available when no suitable track exists.
	preferredLanguage := ""
	for _, stream := range probe.Streams {
		if stream.Index == defaultAudio {
			preferredLanguage = stream.Language
			break
		}
	}
	preferred := -1
	for _, stream := range probe.Streams {
		if stream.StreamType != 2 || !strings.EqualFold(stream.Codec, "aac") {
			continue
		}
		if preferred < 0 {
			preferred = stream.Index
		}
		if preferredLanguage != "" && strings.EqualFold(stream.Language, preferredLanguage) {
			preferred = stream.Index
			break
		}
	}
	if preferred < 0 {
		preferred = defaultAudio
		if preferred < 0 {
			preferred = firstAudio
		}
	}
	probe.PreferredAudioIndex = preferred
	for _, stream := range probe.Streams {
		if stream.StreamType != 2 || stream.Index != preferred {
			continue
		}
		probe.AudioCodec = stream.Codec
		probe.AudioChannels = stream.Channels
		probe.AudioBitrateKbps = stream.BitrateKbps
		probe.AudioProfile = stream.Profile
		probe.AudioSampleRate = stream.SampleRate
		probe.AudioChannelLayout = stream.ChannelLayout
		return
	}
}

func preferredAudioIndex(probe mediaProbe) int {
	if probe.PreferredAudioIndex >= 0 {
		return probe.PreferredAudioIndex
	}
	for _, stream := range probe.Streams {
		if stream.StreamType == 2 {
			return stream.Index
		}
	}
	return -1
}

// directPlaybackNeedsAudioFallback reports codecs that are not consistently
// decodable across Plex clients. The caller may still allow direct playback
// when the client explicitly requested it; this is a conservative fallback,
// not a device-model-specific blacklist.
func directPlaybackNeedsAudioFallback(probe mediaProbe) bool {
	codec := strings.ToLower(strings.TrimSpace(probe.AudioCodec))
	if strings.HasPrefix(codec, "pcm_") {
		return true
	}

	switch codec {
	case "ac3", "dca", "dts", "eac3", "mlp", "truehd":
		return true
	default:
		return false
	}
}

func clientRequestedDirectPlayback(request *http.Request) bool {
	if request == nil || request.URL == nil {
		return false
	}
	return request.URL.Query().Get("directPlay") == "1"
}

func probeStreamType(codecType string) int {
	switch strings.ToLower(strings.TrimSpace(codecType)) {
	case "video":
		return 1
	case "audio":
		return 2
	case "subtitle":
		return 3
	default:
		return 0
	}
}

func plexCodecName(codec string) string {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "dts":
		return "dca"
	case "subrip":
		return "srt"
	case "ass":
		return "ssa"
	case "dvd_subtitle":
		return "dvdsub"
	case "hdmv_pgs_subtitle":
		return "pgs"
	default:
		return strings.TrimSpace(codec)
	}
}

func plexLanguage(language string) string {
	switch strings.ToLower(strings.TrimSpace(language)) {
	case "eng":
		return "en"
	case "zho", "chi":
		return "zh"
	case "jpn":
		return "ja"
	case "kor":
		return "ko"
	case "fra", "fre":
		return "fr"
	case "deu", "ger":
		return "de"
	case "spa":
		return "es"
	case "ita":
		return "it"
	case "rus":
		return "ru"
	default:
		return strings.TrimSpace(language)
	}
}

func normalizeContainer(formatName string) string {
	container := strings.TrimSpace(strings.Split(formatName, ",")[0])
	switch strings.ToLower(container) {
	case "matroska":
		return "mkv"
	case "mov", "mp4", "m4v", "m4a", "3gp", "3g2", "mj2":
		return "mp4"
	default:
		return strings.ToLower(container)
	}
}

func normalizeStreamProfile(codecType, profile string, rawDepth, codedDepth int) string {
	profile = strings.TrimSpace(profile)
	if strings.EqualFold(codecType, "video") {
		lower := strings.ToLower(profile)
		switch {
		case strings.Contains(lower, "baseline"):
			return "baseline"
		case strings.Contains(lower, "high") && strings.Contains(lower, "10"):
			return "high 10"
		case strings.Contains(lower, "high"):
			return "high"
		case strings.Contains(lower, "main") && (strings.Contains(lower, "10") || rawDepth > 8 || codedDepth > 8):
			return "main 10"
		case strings.Contains(lower, "main"):
			return "main"
		}
	}
	if strings.EqualFold(codecType, "audio") {
		lower := strings.ToLower(profile)
		switch {
		case strings.Contains(lower, "dts") && strings.Contains(lower, "ma"):
			return "ma"
		case strings.Contains(lower, "dts") && strings.Contains(lower, "hra"):
			return "hra"
		case strings.Contains(lower, "dts"):
			return "dts"
		case strings.Contains(lower, "low complexity") || strings.Contains(lower, "lc"):
			return "lc"
		case strings.Contains(lower, "high efficiency") || strings.Contains(lower, "he-aac"):
			return "he-aac"
		}
	}
	return profile
}

func parseKbps(value string) int {
	bitrate := parseInt64(value)
	if bitrate <= 0 {
		return 0
	}
	return int(bitrate / 1000)
}

func parseInt(value string) int {
	parsed := parseInt64(value)
	if parsed <= 0 {
		return 0
	}
	return int(parsed)
}

func parseInt64(value string) int64 {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || parsed <= 0 {
		return 0
	}
	return parsed
}

func parseFrameRate(value string) float64 {
	parts := strings.Split(strings.TrimSpace(value), "/")
	if len(parts) != 2 {
		return 0
	}
	numerator, err1 := strconv.ParseFloat(parts[0], 64)
	denominator, err2 := strconv.ParseFloat(parts[1], 64)
	if err1 != nil || err2 != nil || denominator == 0 || numerator <= 0 {
		return 0
	}
	return numerator / denominator
}

func scanType(fieldOrder string) string {
	switch strings.ToLower(strings.TrimSpace(fieldOrder)) {
	case "progressive", "unknown":
		return "progressive"
	case "tt", "bb", "tb", "bt":
		return "interlaced"
	default:
		return ""
	}
}

// probeSTRMMediaForMapping probes the STRM source behind a known PartMapping.
// The URL is validated end to end: runSTRMMediaProbe routes it through
// ResolveMediaTarget (redirect chain plus TargetPolicy) before ffprobe runs,
// so a redirect cannot point the subprocess at a disallowed host.
func (s *Server) probeSTRMMediaForMapping(ctx context.Context, mapping PartMapping) (mediaProbe, bool) {
	return s.probeSTRMMediaURL(ctx, mapping.ResolvedURL)
}

func (s *Server) cachedSTRMMediaProbeForMapping(mapping PartMapping) (mediaProbe, bool) {
	sourceURL := strings.TrimSpace(mapping.ResolvedURL)
	if sourceURL == "" {
		return mediaProbe{}, false
	}
	s.probeMu.Lock()
	entry, ok := s.probes[sourceURL]
	if ok && !time.Now().Before(entry.expiresAt) {
		delete(s.probes, sourceURL)
		ok = false
	}
	s.probeMu.Unlock()
	if !ok {
		return mediaProbe{}, false
	}
	return entry.value, true
}

func (s *Server) probeSTRMMediaURL(ctx context.Context, sourceURL string) (mediaProbe, bool) {
	sourceURL = strings.TrimSpace(sourceURL)
	if sourceURL == "" {
		return mediaProbe{}, false
	}

	s.probeMu.Lock()
	if entry, ok := s.probes[sourceURL]; ok && time.Now().Before(entry.expiresAt) {
		s.probeMu.Unlock()
		return entry.value, true
	}
	if flight, ok := s.probeFlights[sourceURL]; ok {
		s.probeMu.Unlock()
		select {
		case <-flight.done:
			return flight.value, flight.ok
		case <-ctx.Done():
			return mediaProbe{}, false
		}
	}
	flight := &mediaProbeFlight{done: make(chan struct{})}
	s.probeFlights[sourceURL] = flight
	s.probeMu.Unlock()

	probe, ok := s.runSTRMMediaProbe(ctx, sourceURL)
	s.probeMu.Lock()
	if ok {
		s.probes[sourceURL] = mediaProbeCacheEntry{value: probe, expiresAt: time.Now().Add(s.cfg.MappingCacheTTL)}
	}
	flight.value = probe
	flight.ok = ok
	delete(s.probeFlights, sourceURL)
	close(flight.done)
	s.probeMu.Unlock()
	if !ok {
		return mediaProbe{}, false
	}
	s.logger.Info("probed STRM media", "target_host", resolvedTargetHost(sourceURL), "duration_ms", int64(probe.Duration*1000), "stream_count", len(probe.Streams), "video_codec", probe.VideoCodec, "audio_codec", probe.AudioCodec)
	return probe, true
}

// runSTRMMediaProbe resolves and validates the STRM URL's redirect chain
// through ResolveMediaTarget before ffprobe sees it, keeping the subprocess
// on the TargetPolicy-approved final URL.
func (s *Server) runSTRMMediaProbe(ctx context.Context, sourceURL string) (mediaProbe, bool) {
	validatedURL, err := ResolveMediaTarget(ctx, s.mediaClient, s.policy, sourceURL)
	if err != nil {
		s.logger.Warn("failed to validate STRM media target", "target_host", resolvedTargetHost(sourceURL), "error", err)
		return mediaProbe{}, false
	}
	probe, err := probeMedia(ctx, s.cfg.FFmpegPath, validatedURL)
	if err != nil {
		s.logger.Warn("failed to probe STRM media", "target_host", resolvedTargetHost(sourceURL), "error", err)
		return mediaProbe{}, false
	}
	return probe, true
}
