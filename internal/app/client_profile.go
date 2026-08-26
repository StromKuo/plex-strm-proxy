package app

import (
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// ClientPlaybackProfile contains the codec capabilities that Plex clients
// attach to a universal decision request. It is intentionally a partial
// profile: Plex may omit fields, in which case callers should keep the
// copy-first fallback instead of guessing from a user agent or device model.
type ClientPlaybackProfile struct {
	VideoCodecs map[string]bool
	AudioCodecs map[string]bool
	Protocols   map[string]bool
	Containers  map[string]bool
	HasVideo    bool
	HasAudio    bool
}

func parseClientPlaybackProfile(request *http.Request) *ClientPlaybackProfile {
	if request == nil {
		return nil
	}
	values := make([]string, 0, 4)
	if request.URL != nil {
		for key, items := range request.URL.Query() {
			if strings.EqualFold(key, "X-Plex-Client-Profile-Extra") {
				values = append(values, items...)
			}
		}
	}
	if value := strings.TrimSpace(request.Header.Get("X-Plex-Client-Profile-Extra")); value != "" {
		values = append(values, value)
	}
	if len(values) == 0 {
		return nil
	}

	profile := &ClientPlaybackProfile{
		VideoCodecs: make(map[string]bool),
		AudioCodecs: make(map[string]bool),
		Protocols:   make(map[string]bool),
		Containers:  make(map[string]bool),
	}
	for _, value := range values {
		parseClientProfileExtra(value, profile)
	}
	if !profile.HasVideo && !profile.HasAudio && len(profile.Protocols) == 0 && len(profile.Containers) == 0 {
		return nil
	}
	return profile
}

func parseClientProfileExtra(value string, profile *ClientPlaybackProfile) {
	if profile == nil {
		return
	}
	const prefix = "append-transcode-target-codec("
	for {
		start := strings.Index(strings.ToLower(value), prefix)
		if start < 0 {
			return
		}
		payloadStart := start + len(prefix)
		end := strings.Index(value[payloadStart:], ")")
		if end < 0 {
			return
		}
		payload := value[payloadStart : payloadStart+end]
		for _, field := range strings.Split(payload, "&") {
			parts := strings.SplitN(field, "=", 2)
			if len(parts) != 2 {
				continue
			}
			key, err := url.QueryUnescape(strings.TrimSpace(parts[0]))
			if err != nil {
				key = strings.TrimSpace(parts[0])
			}
			text, err := url.QueryUnescape(strings.TrimSpace(parts[1]))
			if err != nil {
				text = strings.TrimSpace(parts[1])
			}
			switch strings.ToLower(key) {
			case "videocodec":
				for _, codec := range splitProfileValues(text) {
					profile.VideoCodecs[normalizePlaybackCodec(codec)] = true
					profile.HasVideo = true
				}
			case "audiocodec":
				for _, codec := range splitProfileValues(text) {
					profile.AudioCodecs[normalizePlaybackCodec(codec)] = true
					profile.HasAudio = true
				}
			case "protocol":
				for _, protocol := range splitProfileValues(text) {
					profile.Protocols[strings.ToLower(protocol)] = true
				}
			case "container":
				for _, container := range splitProfileValues(text) {
					profile.Containers[strings.ToLower(container)] = true
				}
			}
		}
		value = value[payloadStart+end+1:]
	}
}

func splitProfileValues(value string) []string {
	items := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == '|' || r == ';'
	})
	result := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item != "" {
			result = append(result, item)
		}
	}
	return result
}

func normalizePlaybackCodec(codec string) string {
	codec = strings.ToLower(strings.TrimSpace(codec))
	switch {
	case codec == "avc" || codec == "avc1" || codec == "avc3" || codec == "h.264":
		return "h264"
	case codec == "h265" || codec == "h.265" || codec == "hev1" || codec == "hvc1":
		return "hevc"
	case codec == "ec-3":
		return "eac3"
	case strings.HasPrefix(codec, "pcm_"):
		return "pcm"
	default:
		return codec
	}
}

func clientSupportsVideo(profile *ClientPlaybackProfile, codec string) bool {
	if profile == nil || !profile.HasVideo {
		return true
	}
	return profile.VideoCodecs[normalizePlaybackCodec(codec)]
}

func clientSupportsAudio(profile *ClientPlaybackProfile, codec string) bool {
	if profile == nil || !profile.HasAudio {
		return false
	}
	return profile.AudioCodecs[normalizePlaybackCodec(codec)]
}

func sourceVideoNeedsTranscode(profile *ClientPlaybackProfile, probe *mediaProbe) bool {
	if probe == nil || strings.TrimSpace(probe.VideoCodec) == "" {
		return false
	}
	return !clientSupportsVideo(profile, probe.VideoCodec)
}

func sourceAudioNeedsTranscode(profile *ClientPlaybackProfile, probe *mediaProbe) bool {
	if probe == nil || strings.TrimSpace(probe.AudioCodec) == "" {
		return false
	}
	if profile != nil && profile.HasAudio {
		return !clientSupportsAudio(profile, probe.AudioCodec)
	}
	return directPlaybackNeedsAudioFallback(*probe)
}

func playbackProfileValues(values map[string]bool) []string {
	if len(values) == 0 {
		return nil
	}
	result := make([]string, 0, len(values))
	for value, enabled := range values {
		if enabled && strings.TrimSpace(value) != "" {
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}
