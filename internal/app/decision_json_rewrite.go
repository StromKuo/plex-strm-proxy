package app

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
)

func renderHLSTranscodeDecisionJSON(body []byte, mapping PartMapping, query url.Values, sessionID string) ([]byte, bool, error) {
	return renderProxyTranscodeDecisionJSON(body, mapping, query, sessionID, proxyTranscodeHLS)
}

func renderProxyTranscodeDecisionJSON(body []byte, mapping PartMapping, query url.Values, sessionID string, format proxyTranscodeFormat) ([]byte, bool, error) {
	return renderProxyTranscodeDecisionJSONWithPlan(body, mapping, query, sessionID, format, defaultProxyStreamPlan())
}

func renderProxyTranscodeDecisionJSONWithPlan(body []byte, mapping PartMapping, query url.Values, sessionID string, format proxyTranscodeFormat, plan proxyStreamPlan) ([]byte, bool, error) {
	width, height := parseVideoResolution(query.Get("videoResolution"))
	if width <= 0 || height <= 0 {
		width, height = 1280, 720
	}
	bitrate := parseBitrateKbps(query.Get("maxVideoBitrate"))
	var value any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, false, err
	}
	changed := rewriteProxyTranscodeJSONNode(value, mapping, width, height, bitrate, sessionID, format, plan)
	if !changed {
		return body, false, nil
	}
	rewritten, err := json.Marshal(value)
	if err != nil {
		return nil, false, err
	}
	return rewritten, true, nil
}

func rewriteProxyTranscodeJSONNode(node any, mapping PartMapping, width, height, bitrate int, sessionID string, format proxyTranscodeFormat, plan proxyStreamPlan) bool {
	switch typed := node.(type) {
	case []any:
		changed := false
		for _, child := range typed {
			changed = rewriteProxyTranscodeJSONNode(child, mapping, width, height, bitrate, sessionID, format, plan) || changed
		}
		return changed
	case map[string]any:
		changed := false
		if _, isMediaContainer := objectValue(typed, "Metadata"); isMediaContainer {
			setDecisionJSONValue(typed, "directPlayDecisionCode", 3000)
			setDecisionJSONValue(typed, "directPlayDecisionText", "App cannot direct play this item. Direct play is disabled.")
			setDecisionJSONValue(typed, "generalDecisionCode", 1001)
			setDecisionJSONValue(typed, "generalDecisionText", "Direct play not available; Conversion OK.")
			setDecisionJSONValue(typed, "resourceSession", sessionID)
			setDecisionJSONValue(typed, "transcodeDecisionCode", 1001)
			setDecisionJSONValue(typed, "transcodeDecisionText", "Direct play not available; Conversion OK.")
			changed = true
		}
		for key, child := range typed {
			if strings.EqualFold(key, "Media") {
				changed = rewriteProxyTranscodeJSONMedia(child, mapping, width, height, bitrate, sessionID, format, plan) || changed
				continue
			}
			changed = rewriteProxyTranscodeJSONNode(child, mapping, width, height, bitrate, sessionID, format, plan) || changed
		}
		return changed
	default:
		return false
	}
}

func rewriteProxyTranscodeJSONMedia(value any, mapping PartMapping, width, height, bitrate int, sessionID string, format proxyTranscodeFormat, plan proxyStreamPlan) bool {
	container, protocol, partKey := proxyDecisionMediaAttributes(format, sessionID)
	mediaList := []any{value}
	if list, ok := value.([]any); ok {
		mediaList = list
	}
	changed := false
	for _, item := range mediaList {
		media, ok := item.(map[string]any)
		if !ok {
			continue
		}
		partValue, ok := objectValue(media, "Part")
		if !ok {
			continue
		}
		partList := []any{partValue}
		if list, ok := partValue.([]any); ok {
			partList = list
		}
		for _, partItem := range partList {
			part, ok := partItem.(map[string]any)
			if !ok || !decisionJSONPartMatches(part, mapping) {
				continue
			}
			setDecisionJSONValue(media, "bitrate", bitrate)
			setDecisionJSONValue(media, "container", container)
			setDecisionJSONValue(media, "protocol", protocol)
			if plan.VideoCodec != "" {
				setDecisionJSONValue(media, "videoCodec", plan.VideoCodec)
			}
			if plan.AudioCodec != "" {
				setDecisionJSONValue(media, "audioCodec", plan.AudioCodec)
			}
			setDecisionJSONValue(media, "width", width)
			setDecisionJSONValue(media, "height", height)
			setDecisionJSONValue(media, "selected", true)
			removeJSONValue(part, "file")
			setDecisionJSONValue(part, "key", partKey)
			setDecisionJSONValue(part, "bitrate", bitrate)
			setDecisionJSONValue(part, "container", container)
			setDecisionJSONValue(part, "protocol", protocol)
			setDecisionJSONValue(part, "decision", "transcode")
			setDecisionJSONValue(part, "width", width)
			setDecisionJSONValue(part, "height", height)
			setDecisionJSONValue(part, "selected", true)
			videoStream := map[string]any{"streamType": 1, "width": width, "height": height, "decision": plan.VideoDecision, "location": plan.VideoLocation, "selected": true}
			if plan.VideoCodec != "" {
				videoStream["codec"] = plan.VideoCodec
			}
			audioStream := map[string]any{"streamType": 2, "channels": 2, "decision": plan.AudioDecision, "location": plan.AudioLocation, "selected": true}
			if plan.AudioCodec != "" {
				audioStream["codec"] = plan.AudioCodec
			}
			setDecisionJSONValue(part, "Stream", []any{videoStream, audioStream})
			changed = true
		}
	}
	return changed
}

func removeJSONValue(object map[string]any, name string) {
	for key := range object {
		if strings.EqualFold(key, name) {
			delete(object, key)
		}
	}
}

func rewriteSTRMDirectDecisionJSON(body []byte, mapping PartMapping) ([]byte, bool, error) {
	return rewriteSTRMDirectDecisionJSONWithProbe(body, mapping, nil)
}

func rewriteSTRMDirectDecisionJSONWithProbe(body []byte, mapping PartMapping, probe *mediaProbe) ([]byte, bool, error) {
	if mapping.Kind != PartKindSTRM || mapping.PartID == "" || mapping.ResolvedURL == "" {
		return body, false, nil
	}
	container := mediaContainerForURL(mapping.ResolvedURL)
	if container == "" {
		return body, false, nil
	}

	var value any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, false, err
	}
	changed := rewriteSTRMDirectDecisionJSONNode(value, mapping, container, probe)
	if !changed {
		return body, false, nil
	}
	rewritten, err := json.Marshal(value)
	if err != nil {
		return nil, false, err
	}
	return rewritten, true, nil
}

// rewriteSTRMNativeDirectDecisionJSON keeps Plex's native direct decision
// intact and only replaces the proxy-local Part file URL with the resolved
// source URL visible to the playback client.
func rewriteSTRMNativeDirectDecisionJSON(body []byte, mapping PartMapping) ([]byte, bool, error) {
	if mapping.Kind != PartKindSTRM || mapping.PartID == "" || mapping.ResolvedURL == "" {
		return body, false, nil
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, false, err
	}
	if !rewriteSTRMNativeDirectDecisionJSONNode(value, mapping) {
		return body, false, nil
	}
	rewritten, err := json.Marshal(value)
	if err != nil {
		return nil, false, err
	}
	return rewritten, true, nil
}

func rewriteSTRMNativeDirectDecisionJSONNode(node any, mapping PartMapping) bool {
	switch typed := node.(type) {
	case []any:
		changed := false
		for _, child := range typed {
			changed = rewriteSTRMNativeDirectDecisionJSONNode(child, mapping) || changed
		}
		return changed
	case map[string]any:
		changed := false
		for key, child := range typed {
			if strings.EqualFold(key, "Part") {
				changed = rewriteSTRMNativeDirectDecisionJSONParts(child, mapping) || changed
				continue
			}
			changed = rewriteSTRMNativeDirectDecisionJSONNode(child, mapping) || changed
		}
		return changed
	default:
		return false
	}
}

func rewriteSTRMNativeDirectDecisionJSONParts(value any, mapping PartMapping) bool {
	parts := []any{value}
	if list, ok := value.([]any); ok {
		parts = list
	}
	changed := false
	for _, item := range parts {
		part, ok := item.(map[string]any)
		if !ok || !decisionJSONPartMatches(part, mapping) {
			continue
		}
		setDecisionJSONValue(part, "file", mapping.ResolvedURL)
		changed = true
	}
	return changed
}

func rewriteSTRMDirectDecisionJSONNode(node any, mapping PartMapping, container string, probe *mediaProbe) bool {
	switch typed := node.(type) {
	case []any:
		changed := false
		for _, child := range typed {
			changed = rewriteSTRMDirectDecisionJSONNode(child, mapping, container, probe) || changed
		}
		return changed
	case map[string]any:
		changed := false
		if _, isMediaContainer := objectValue(typed, "Metadata"); isMediaContainer {
			changed = rewriteSTRMDirectDecisionJSONContainer(typed) || changed
		}
		for key, child := range typed {
			if strings.EqualFold(key, "Media") {
				changed = rewriteSTRMDirectDecisionJSONMedia(child, mapping, container, probe) || changed
				continue
			}
			changed = rewriteSTRMDirectDecisionJSONNode(child, mapping, container, probe) || changed
		}
		return changed
	default:
		return false
	}
}

func rewriteSTRMDirectDecisionJSONContainer(object map[string]any) bool {
	setDecisionJSONValue(object, "mdeDecisionCode", 1000)
	setDecisionJSONValue(object, "mdeDecisionText", "Direct play is available for the resolved STRM source.")
	setDecisionJSONValue(object, "directPlayDecisionCode", 1000)
	setDecisionJSONValue(object, "directPlayDecisionText", "Direct play is available for the resolved STRM source.")
	setDecisionJSONValue(object, "generalDecisionCode", 1000)
	setDecisionJSONValue(object, "generalDecisionText", "Direct play is available for the resolved STRM source.")
	setDecisionJSONValue(object, "transcodeDecisionCode", 1000)
	setDecisionJSONValue(object, "transcodeDecisionText", "Direct play is available for the resolved STRM source.")
	return true
}

func rewriteSTRMDirectDecisionJSONMedia(value any, mapping PartMapping, container string, probe *mediaProbe) bool {
	mediaList := []any{value}
	if list, ok := value.([]any); ok {
		mediaList = list
	}
	changed := false
	for _, item := range mediaList {
		media, ok := item.(map[string]any)
		if !ok {
			continue
		}
		partValue, ok := objectValue(media, "Part")
		if !ok {
			continue
		}
		partList := []any{partValue}
		if list, ok := partValue.([]any); ok {
			partList = list
		}
		for _, item := range partList {
			part, ok := item.(map[string]any)
			if !ok || !decisionJSONPartMatches(part, mapping) {
				continue
			}
			setDecisionJSONValue(media, "container", container)
			setDecisionJSONValue(media, "protocol", "http")
			setDecisionJSONValue(media, "videoDecision", "directplay")
			setDecisionJSONValue(media, "audioDecision", "directplay")
			setDecisionJSONValue(media, "selected", true)
			if probe != nil {
				setProbeJSONAttributes(media, probe, true)
			}

			setDecisionJSONValue(part, "container", container)
			setDecisionJSONValue(part, "file", mapping.ResolvedURL)
			setDecisionJSONValue(part, "protocol", "http")
			setDecisionJSONValue(part, "decision", "directplay")
			setDecisionJSONValue(part, "key", directPartKey(mapping))
			setDecisionJSONValue(part, "selected", true)
			rewriteSTRMDirectDecisionJSONStreams(part, probe)
			changed = true
		}
	}
	return changed
}

func decisionJSONPartMatches(part map[string]any, mapping PartMapping) bool {
	record := partRecordFromJSONObject(part)
	if record.ID != "" {
		return record.ID == mapping.PartID
	}
	return jsonObjectContainsSTRM(part)
}

func rewriteSTRMDirectDecisionJSONStreams(part map[string]any, probe *mediaProbe) {
	value, ok := objectValue(part, "Stream")
	if !ok {
		if probe != nil {
			part["Stream"] = directJSONStreams(*probe)
		}
		return
	}
	streams := []any{value}
	if list, ok := value.([]any); ok {
		streams = list
	}
	hasVideoStream := false
	hasAudioStream := false
	preferredAudio := -1
	if probe != nil {
		preferredAudio = preferredAudioIndex(*probe)
	}
	for _, item := range streams {
		stream, ok := item.(map[string]any)
		if !ok {
			continue
		}
		setDecisionJSONValue(stream, "decision", "directplay")
		setDecisionJSONValue(stream, "location", "direct")
		streamType := jsonString(jsonObjectValue(stream, "streamType"))
		selected := streamType == "1"
		if streamType == "2" {
			streamIndex, err := strconv.Atoi(jsonString(jsonObjectValue(stream, "index")))
			if err == nil && preferredAudio >= 0 {
				selected = streamIndex == preferredAudio
			} else {
				// Some Plex responses omit the source stream index. Preserve
				// the server's existing selection in that case.
				selected = jsonBool(jsonObjectValue(stream, "selected"))
			}
		}
		setDecisionJSONValue(stream, "selected", selected)
		switch streamType {
		case "1":
			hasVideoStream = true
			if probe != nil && probe.VideoCodec != "" {
				setDecisionJSONValue(stream, "codec", probe.VideoCodec)
			}
			if probe != nil {
				if probe.Width > 0 {
					setDecisionJSONValue(stream, "width", probe.Width)
				}
				if probe.Height > 0 {
					setDecisionJSONValue(stream, "height", probe.Height)
				}
			}
		case "2":
			hasAudioStream = true
			if probe != nil && probe.AudioCodec != "" {
				setDecisionJSONValue(stream, "codec", probe.AudioCodec)
			}
			if probe != nil && probe.AudioChannels > 0 {
				setDecisionJSONValue(stream, "channels", probe.AudioChannels)
			}
		}
	}
	if list, ok := value.([]any); ok && probe != nil {
		needVideo := !hasVideoStream
		needAudio := !hasAudioStream
		for _, item := range directJSONStreams(*probe) {
			stream, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch jsonString(jsonObjectValue(stream, "streamType")) {
			case "1":
				if !needVideo {
					continue
				}
			case "2":
				if !needAudio {
					continue
				}
			}
			list = append(list, stream)
		}
		setJSONObjectValueAny(part, "Stream", list)
	}
}

func directJSONStreams(probe mediaProbe) []any {
	streams := metadataJSONStreams(probe)
	preferredAudio := preferredAudioIndex(probe)
	audioSelected := false
	for _, item := range streams {
		stream, ok := item.(map[string]any)
		if !ok {
			continue
		}
		setDecisionJSONValue(stream, "location", "direct")
		setDecisionJSONValue(stream, "decision", "directplay")
		streamType := jsonString(jsonObjectValue(stream, "streamType"))
		selected := streamType == "1"
		if streamType == "2" {
			streamIndex, err := strconv.Atoi(jsonString(jsonObjectValue(stream, "index")))
			selected = (err == nil && preferredAudio >= 0 && streamIndex == preferredAudio) ||
				(preferredAudio < 0 && !audioSelected)
			audioSelected = audioSelected || selected
		}
		setDecisionJSONValue(stream, "selected", selected)
	}
	return streams
}

func directJSONVideoStream(probe mediaProbe) map[string]any {
	stream := map[string]any{"streamType": 1, "location": "direct", "decision": "directplay", "selected": true}
	if probe.VideoCodec != "" {
		stream["codec"] = probe.VideoCodec
	}
	if probe.Width > 0 {
		stream["width"] = probe.Width
	}
	if probe.Height > 0 {
		stream["height"] = probe.Height
	}
	if probe.BitrateKbps > 0 {
		stream["bitrate"] = probe.BitrateKbps
	}
	setProbeJSONStreamAttributes(stream, &probe, "1")
	return stream
}

func directJSONAudioStream(probe mediaProbe) map[string]any {
	stream := map[string]any{"streamType": 2, "location": "direct", "decision": "directplay", "selected": true}
	if probe.AudioCodec != "" {
		stream["codec"] = probe.AudioCodec
	}
	if probe.AudioChannels > 0 {
		stream["channels"] = probe.AudioChannels
	}
	setProbeJSONStreamAttributes(stream, &probe, "2")
	return stream
}

func setProbeJSONAttributes(object map[string]any, probe *mediaProbe, media bool) {
	if probe == nil {
		return
	}
	if probe.VideoCodec != "" {
		setDecisionJSONValue(object, "videoCodec", probe.VideoCodec)
	}
	if probe.AudioCodec != "" {
		setDecisionJSONValue(object, "audioCodec", probe.AudioCodec)
	}
	if probe.Width > 0 {
		setDecisionJSONValue(object, "width", probe.Width)
	}
	if probe.Height > 0 {
		setDecisionJSONValue(object, "height", probe.Height)
	}
	if probe.AudioChannels > 0 {
		setDecisionJSONValue(object, "audioChannels", probe.AudioChannels)
	}
	if probe.Duration > 0 {
		setDecisionJSONValue(object, "duration", int64(probe.Duration*1000))
	}
	if media && probe.BitrateKbps > 0 {
		setDecisionJSONValue(object, "bitrate", probe.BitrateKbps)
	}
	if probe.VideoFrameRate > 0 {
		// Keep the Plex JSON representation: frame rates are strings such as
		// "25.000", not JSON numbers.
		setDecisionJSONValue(object, "videoFrameRate", strconv.FormatFloat(probe.VideoFrameRate, 'f', 3, 64))
	}
	if probe.VideoProfile != "" {
		setDecisionJSONValue(object, "videoProfile", probe.VideoProfile)
	}
	if probe.AudioProfile != "" {
		setDecisionJSONValue(object, "audioProfile", probe.AudioProfile)
	}
	if probe.VideoBitDepth > 0 {
		setDecisionJSONValue(object, "bitDepth", probe.VideoBitDepth)
	}
	if probe.AudioSampleRate > 0 {
		setDecisionJSONValue(object, "audioSampleRate", probe.AudioSampleRate)
	}
	if probe.ColorRange != "" {
		setDecisionJSONValue(object, "colorRange", probe.ColorRange)
	}
	if probe.ColorSpace != "" {
		setDecisionJSONValue(object, "colorSpace", probe.ColorSpace)
	}
	if probe.ColorTransfer != "" {
		setDecisionJSONValue(object, "colorTransfer", probe.ColorTransfer)
	}
	if probe.ColorPrimaries != "" {
		setDecisionJSONValue(object, "colorPrimaries", probe.ColorPrimaries)
	}
}

func setDecisionJSONValue(object map[string]any, name string, value any) {
	for key := range object {
		if strings.EqualFold(key, name) {
			object[key] = value
			return
		}
	}
	object[name] = value
}

func jsonObjectValue(object map[string]any, name string) any {
	value, ok := objectValue(object, name)
	if !ok {
		return nil
	}
	return value
}

func isJSONContentType(contentType string) bool {
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	return mediaType == "application/json" || mediaType == "text/json"
}
