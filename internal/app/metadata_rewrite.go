package app

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"encoding/xml"
	"io"
	"strconv"
	"strings"
)

type metadataProbeFunc func(part partRecord) *mediaProbe

func rewriteSTRMMetadata(contentType string, body []byte) ([]byte, bool, error) {
	return rewriteSTRMMetadataWithContainerAndFileAndProbe(contentType, body, func(part partRecord) string {
		return "mp4"
	}, nil, nil)
}

func rewriteSTRMMetadataWithContainer(contentType string, body []byte, containerFor func(part partRecord) string) ([]byte, bool, error) {
	return rewriteSTRMMetadataWithContainerAndFileAndProbe(contentType, body, containerFor, nil, nil)
}

func rewriteSTRMMetadataWithContainerAndFile(contentType string, body []byte, containerFor func(part partRecord) string, fileFor func(part partRecord) string) ([]byte, bool, error) {
	return rewriteSTRMMetadataWithContainerAndFileAndProbe(contentType, body, containerFor, fileFor, nil)
}

func rewriteSTRMMetadataWithContainerAndFileAndProbe(contentType string, body []byte, containerFor func(part partRecord) string, fileFor func(part partRecord) string, probeFor metadataProbeFunc) ([]byte, bool, error) {
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	switch mediaType {
	case "application/xml", "text/xml", "application/plex+xml":
		return rewriteSTRMXMLMetadataWithFileAndProbe(body, containerFor, fileFor, probeFor)
	case "application/json", "text/json":
		return rewriteSTRMJSONMetadataWithFileAndProbe(body, containerFor, fileFor, probeFor)
	default:
		return body, false, nil
	}
}

func encodeStructuredBody(contentEncoding string, body []byte) ([]byte, error) {
	encoding := strings.ToLower(strings.TrimSpace(strings.Split(contentEncoding, ",")[0]))
	if encoding != "gzip" {
		return body, nil
	}
	var encoded bytes.Buffer
	writer := gzip.NewWriter(&encoded)
	if _, err := writer.Write(body); err != nil {
		_ = writer.Close()
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return encoded.Bytes(), nil
}

func rewriteSTRMXMLMetadata(body []byte, containerFor func(part partRecord) string) ([]byte, bool, error) {
	return rewriteSTRMXMLMetadataWithFileAndProbe(body, containerFor, nil, nil)
}

func rewriteSTRMXMLMetadataWithFile(body []byte, containerFor func(part partRecord) string, fileFor func(part partRecord) string) ([]byte, bool, error) {
	return rewriteSTRMXMLMetadataWithFileAndProbe(body, containerFor, fileFor, nil)
}

func rewriteSTRMXMLMetadataWithFileAndProbe(body []byte, containerFor func(part partRecord) string, fileFor func(part partRecord) string, probeFor metadataProbeFunc) ([]byte, bool, error) {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	tokens := make([]xml.Token, 0, 32)
	mediaStarts := make([]int, 0, 2)
	videoStarts := make([]int, 0, 2)
	targetPart := false
	partHasVideoStream := false
	partHasAudioStream := false
	partProbe := (*mediaProbe)(nil)
	changed := false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, false, err
		}
		switch typed := token.(type) {
		case xml.StartElement:
			start := cloneXMLStartElement(typed)
			tokens = append(tokens, start)
			if start.Name.Local == "Stream" && targetPart {
				switch xmlAttributeValue(start.Attr, "streamType") {
				case "1":
					partHasVideoStream = true
				case "2":
					partHasAudioStream = true
				}
				if partProbe != nil {
					setProbeXMLStreamAttributes(&start, *partProbe, xmlAttributeValue(start.Attr, "streamType"))
					tokens[len(tokens)-1] = start
					changed = true
				}
			}
			if start.Name.Local == "Video" {
				videoStarts = append(videoStarts, len(tokens)-1)
			}
			if start.Name.Local == "Media" {
				mediaStarts = append(mediaStarts, len(tokens)-1)
				continue
			}
			if start.Name.Local != "Part" || len(mediaStarts) == 0 || !xmlAttributesContainSTRM(start.Attr) {
				continue
			}
			targetPart = true
			partHasVideoStream = false
			partHasAudioStream = false
			part := partRecordFromXMLAttrs(start.Attr)
			if probeFor != nil {
				partProbe = probeFor(part)
			}
			container := ""
			if containerFor != nil {
				container = containerFor(part)
			}
			mediaIndex := mediaStarts[len(mediaStarts)-1]
			if partProbe != nil {
				media := tokens[mediaIndex].(xml.StartElement)
				setProbeXMLAttributes(&media, *partProbe, true)
				tokens[mediaIndex] = media
				if len(videoStarts) > 0 {
					video := tokens[videoStarts[len(videoStarts)-1]].(xml.StartElement)
					setProbeXMLAttributes(&video, *partProbe, false)
					tokens[videoStarts[len(videoStarts)-1]] = video
				}
				setProbeXMLAttributes(&start, *partProbe, false)
				tokens[len(tokens)-1] = start
				changed = true
			}
			if container != "" {
				media := tokens[mediaIndex].(xml.StartElement)
				setXMLAttribute(&media, "container", container)
				tokens[mediaIndex] = media
				rewrittenPart := tokens[len(tokens)-1].(xml.StartElement)
				setXMLAttribute(&rewrittenPart, "container", container)
				tokens[len(tokens)-1] = rewrittenPart
				changed = true
			}
			if fileFor != nil {
				file := strings.TrimSpace(fileFor(part))
				if file != "" {
					rewrittenPart := tokens[len(tokens)-1].(xml.StartElement)
					// The Part is now backed by the resolved media URL. Declare
					// the same direct-play contract here as in the decision
					// response, so clients that start from a hub or play queue do
					// not select HLS before requesting the Part key. Keep the
					// canonical Part key as the proxy's 302 entry point, while
					// exposing the resolved URL in file. Android uses the file
					// extension when deciding whether this is a native media
					// resource or an HLS/transcode input.
					setXMLAttribute(&rewrittenPart, "file", file)
					setXMLAttribute(&rewrittenPart, "protocol", "http")
					setXMLAttribute(&rewrittenPart, "decision", "directplay")
					setXMLAttribute(&rewrittenPart, "key", directPartKey(PartMapping{PartID: part.ID, Key: part.Key}))
					setXMLAttribute(&rewrittenPart, "selected", "1")
					tokens[len(tokens)-1] = rewrittenPart
					media := tokens[mediaIndex].(xml.StartElement)
					setXMLAttribute(&media, "protocol", "http")
					setXMLAttribute(&media, "videoDecision", "directplay")
					setXMLAttribute(&media, "audioDecision", "directplay")
					tokens[mediaIndex] = media
					changed = true
				}
			}
		case xml.EndElement:
			if typed.Name.Local == "Part" && targetPart && partProbe != nil {
				for _, stream := range metadataXMLStreams(*partProbe, partHasVideoStream, partHasAudioStream) {
					tokens = append(tokens, stream, xml.EndElement{Name: xml.Name{Local: "Stream"}})
					changed = true
				}
			}
			tokens = append(tokens, xml.CopyToken(typed))
			if typed.Name.Local == "Part" {
				targetPart = false
				partHasVideoStream = false
				partHasAudioStream = false
				partProbe = nil
			}
			if typed.Name.Local == "Media" && len(mediaStarts) > 0 {
				mediaStarts = mediaStarts[:len(mediaStarts)-1]
			}
			if typed.Name.Local == "Video" && len(videoStarts) > 0 {
				videoStarts = videoStarts[:len(videoStarts)-1]
			}
		default:
			tokens = append(tokens, xml.CopyToken(token))
		}
	}
	if !changed {
		return body, false, nil
	}

	var rewritten bytes.Buffer
	encoder := xml.NewEncoder(&rewritten)
	for _, token := range tokens {
		if err := encoder.EncodeToken(token); err != nil {
			return nil, false, err
		}
	}
	if err := encoder.Flush(); err != nil {
		return nil, false, err
	}
	return rewritten.Bytes(), true, nil
}

func metadataXMLVideoStream(probe mediaProbe) xml.StartElement {
	start := xml.StartElement{Name: xml.Name{Local: "Stream"}}
	setXMLAttribute(&start, "id", strconv.Itoa(syntheticStreamID(0)))
	setXMLAttribute(&start, "streamType", "1")
	setXMLAttribute(&start, "default", "1")
	setProbeXMLStreamAttributes(&start, probe, "1")
	return start
}

func metadataXMLAudioStream(probe mediaProbe) xml.StartElement {
	start := xml.StartElement{Name: xml.Name{Local: "Stream"}}
	setXMLAttribute(&start, "id", strconv.Itoa(syntheticStreamID(1)))
	setXMLAttribute(&start, "streamType", "2")
	setXMLAttribute(&start, "selected", "1")
	setProbeXMLStreamAttributes(&start, probe, "2")
	return start
}

func metadataXMLStreams(probe mediaProbe, hasVideo, hasAudio bool) []xml.StartElement {
	if len(probe.Streams) == 0 {
		streams := make([]xml.StartElement, 0, 2)
		if !hasVideo {
			streams = append(streams, metadataXMLVideoStream(probe))
		}
		if !hasAudio {
			streams = append(streams, metadataXMLAudioStream(probe))
		}
		return streams
	}
	result := make([]xml.StartElement, 0, len(probe.Streams))
	needVideo := !hasVideo
	needAudio := !hasAudio
	audioSelected := hasAudio
	preferredAudio := preferredAudioIndex(probe)
	for _, stream := range probe.Streams {
		if stream.StreamType == 1 && !needVideo || stream.StreamType == 2 && !needAudio {
			continue
		}
		start := metadataXMLStream(stream)
		if stream.StreamType == 2 && (stream.Index == preferredAudio || (preferredAudio < 0 && !audioSelected)) {
			setXMLAttribute(&start, "selected", "1")
			audioSelected = true
		}
		result = append(result, start)
	}
	return result
}

func metadataXMLStream(stream mediaProbeStream) xml.StartElement {
	start := xml.StartElement{Name: xml.Name{Local: "Stream"}}
	setXMLAttribute(&start, "id", strconv.Itoa(syntheticStreamID(stream.Index)))
	setXMLAttribute(&start, "streamType", strconv.Itoa(stream.StreamType))
	if stream.Codec != "" {
		setXMLAttribute(&start, "codec", stream.Codec)
	}
	if stream.Index >= 0 {
		setXMLAttribute(&start, "index", strconv.Itoa(stream.Index))
	}
	if stream.Language != "" {
		setXMLAttribute(&start, "language", displayLanguage(stream.Language))
		setXMLAttribute(&start, "languageTag", stream.Language)
	}
	if stream.Title != "" {
		setXMLAttribute(&start, "title", stream.Title)
	}
	if stream.Default {
		setXMLAttribute(&start, "default", "1")
	}
	if stream.Forced {
		setXMLAttribute(&start, "forced", "1")
	}
	setXMLMetadataStreamAttributes(&start, stream)
	return start
}

func setXMLMetadataStreamAttributes(start *xml.StartElement, stream mediaProbeStream) {
	if stream.Width > 0 {
		setXMLAttribute(start, "width", strconv.Itoa(stream.Width))
	}
	if stream.Height > 0 {
		setXMLAttribute(start, "height", strconv.Itoa(stream.Height))
	}
	if stream.CodedWidth > 0 {
		setXMLAttribute(start, "codedWidth", strconv.Itoa(stream.CodedWidth))
	}
	if stream.CodedHeight > 0 {
		setXMLAttribute(start, "codedHeight", strconv.Itoa(stream.CodedHeight))
	}
	if stream.BitrateKbps > 0 {
		setXMLAttribute(start, "bitrate", strconv.Itoa(stream.BitrateKbps))
	}
	if stream.FrameRate > 0 {
		setXMLAttribute(start, "frameRate", strconv.FormatFloat(stream.FrameRate, 'f', 3, 64))
	}
	if stream.BitDepth > 0 {
		setXMLAttribute(start, "bitDepth", strconv.Itoa(stream.BitDepth))
	}
	if stream.ScanType != "" {
		setXMLAttribute(start, "scanType", stream.ScanType)
	}
	if stream.Channels > 0 {
		setXMLAttribute(start, "channels", strconv.Itoa(stream.Channels))
	}
	if stream.SampleRate > 0 {
		setXMLAttribute(start, "samplingRate", strconv.Itoa(stream.SampleRate))
	}
	if stream.ChannelLayout != "" {
		setXMLAttribute(start, "audioChannelLayout", stream.ChannelLayout)
	}
	if stream.ColorRange != "" {
		setXMLAttribute(start, "colorRange", stream.ColorRange)
	}
	if stream.ColorSpace != "" {
		setXMLAttribute(start, "colorSpace", stream.ColorSpace)
	}
	if stream.ColorTransfer != "" {
		setXMLAttribute(start, "colorTransfer", stream.ColorTransfer)
	}
	if stream.ColorPrimaries != "" {
		setXMLAttribute(start, "colorPrimaries", stream.ColorPrimaries)
	}
}

func cloneXMLStartElement(start xml.StartElement) xml.StartElement {
	start.Attr = append([]xml.Attr(nil), start.Attr...)
	return start
}

func addXMLAttribute(start xml.StartElement, name, value string) xml.StartElement {
	for _, attribute := range start.Attr {
		if strings.EqualFold(attribute.Name.Local, name) {
			return start
		}
	}
	start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: name}, Value: value})
	return start
}

func xmlAttributesContainSTRM(attributes []xml.Attr) bool {
	for _, attribute := range attributes {
		switch strings.ToLower(attribute.Name.Local) {
		case "file", "path", "key":
			if isSTRMPath(attribute.Value) {
				return true
			}
		}
	}
	return false
}

func rewriteSTRMJSONMetadata(body []byte, containerFor func(part partRecord) string) ([]byte, bool, error) {
	return rewriteSTRMJSONMetadataWithFileAndProbe(body, containerFor, nil, nil)
}

func rewriteSTRMJSONMetadataWithFile(body []byte, containerFor func(part partRecord) string, fileFor func(part partRecord) string) ([]byte, bool, error) {
	return rewriteSTRMJSONMetadataWithFileAndProbe(body, containerFor, fileFor, nil)
}

func rewriteSTRMJSONMetadataWithFileAndProbe(body []byte, containerFor func(part partRecord) string, fileFor func(part partRecord) string, probeFor metadataProbeFunc) ([]byte, bool, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, false, err
	}
	changed := rewriteSTRMJSONNodeWithFileAndProbe(value, containerFor, fileFor, probeFor)
	if !changed {
		return body, false, nil
	}
	rewritten, err := json.Marshal(value)
	if err != nil {
		return nil, false, err
	}
	return rewritten, true, nil
}

func rewriteSTRMJSONNode(node any, containerFor func(part partRecord) string) bool {
	return rewriteSTRMJSONNodeWithFileAndProbe(node, containerFor, nil, nil)
}

func rewriteSTRMJSONNodeWithFile(node any, containerFor func(part partRecord) string, fileFor func(part partRecord) string) bool {
	return rewriteSTRMJSONNodeWithFileAndProbe(node, containerFor, fileFor, nil)
}

func rewriteSTRMJSONNodeWithFileAndProbe(node any, containerFor func(part partRecord) string, fileFor func(part partRecord) string, probeFor metadataProbeFunc) bool {
	switch typed := node.(type) {
	case []any:
		changed := false
		for _, child := range typed {
			changed = rewriteSTRMJSONNodeWithFileAndProbe(child, containerFor, fileFor, probeFor) || changed
		}
		return changed
	case map[string]any:
		changed := false
		for key, child := range typed {
			if strings.EqualFold(key, "Media") {
				changed = rewriteSTRMJSONMediaWithFileAndProbe(child, containerFor, fileFor, probeFor) || changed
				if probeFor != nil {
					if probe := firstSTRMJSONProbe(child, probeFor); probe != nil {
						setProbeJSONAttributes(typed, probe, false)
						changed = true
					}
				}
				continue
			}
			changed = rewriteSTRMJSONNodeWithFileAndProbe(child, containerFor, fileFor, probeFor) || changed
		}
		return changed
	default:
		return false
	}
}

func rewriteSTRMJSONMedia(value any, containerFor func(part partRecord) string) bool {
	return rewriteSTRMJSONMediaWithFileAndProbe(value, containerFor, nil, nil)
}

func rewriteSTRMJSONMediaWithFile(value any, containerFor func(part partRecord) string, fileFor func(part partRecord) string) bool {
	return rewriteSTRMJSONMediaWithFileAndProbe(value, containerFor, fileFor, nil)
}

func rewriteSTRMJSONMediaWithFileAndProbe(value any, containerFor func(part partRecord) string, fileFor func(part partRecord) string, probeFor metadataProbeFunc) bool {
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
		for _, partValue := range partList {
			part, ok := partValue.(map[string]any)
			if !ok || !jsonObjectContainsSTRM(part) {
				continue
			}
			partChanged := false
			partRecord := partRecordFromJSONObject(part)
			container := ""
			if containerFor != nil {
				container = containerFor(partRecord)
			}
			if container != "" {
				setJSONObjectValue(media, "container", container)
				setJSONObjectValue(part, "container", container)
				partChanged = true
			}
			if fileFor != nil {
				file := strings.TrimSpace(fileFor(partRecord))
				if file != "" {
					setJSONObjectValue(part, "file", file)
					setJSONObjectValue(media, "protocol", "http")
					setJSONObjectValue(media, "videoDecision", "directplay")
					setJSONObjectValue(media, "audioDecision", "directplay")
					setJSONObjectValue(part, "protocol", "http")
					setJSONObjectValue(part, "decision", "directplay")
					setJSONObjectValue(part, "key", directPartKey(PartMapping{PartID: partRecord.ID, Key: partRecord.Key}))
					setJSONObjectValueAny(part, "selected", true)
					partChanged = true
				}
			}
			var probe *mediaProbe
			if probeFor != nil {
				probe = probeFor(partRecord)
			}
			if probe != nil {
				setProbeJSONAttributes(media, probe, true)
				setProbeJSONAttributes(part, probe, false)
			}
			if probe != nil {
				if value, exists := objectValue(part, "Stream"); exists {
					streams := jsonStreamList(value)
					hasVideo, hasAudio := rewriteSTRMJSONMetadataStreams(streams, probe)
					needVideo := !hasVideo
					needAudio := !hasAudio
					for _, stream := range metadataJSONStreams(*probe) {
						streamObject, ok := stream.(map[string]any)
						if !ok {
							continue
						}
						switch jsonString(jsonObjectValue(streamObject, "streamType")) {
						case "1":
							if !needVideo {
								continue
							}
						case "2":
							if !needAudio {
								continue
							}
						}
						streams = append(streams, stream)
					}
					setJSONObjectValueAny(part, "Stream", streams)
				} else {
					part["Stream"] = metadataJSONStreams(*probe)
				}
				partChanged = true
			} else if partChanged {
				part["Stream"] = []any{}
			}
			changed = changed || partChanged
		}
	}
	return changed
}

func firstSTRMJSONProbe(value any, probeFor metadataProbeFunc) *mediaProbe {
	mediaList := []any{value}
	if list, ok := value.([]any); ok {
		mediaList = list
	}
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
			if !ok || !jsonObjectContainsSTRM(part) {
				continue
			}
			if probe := probeFor(partRecordFromJSONObject(part)); probe != nil {
				return probe
			}
		}
	}
	return nil
}

func jsonStreamList(value any) []any {
	if list, ok := value.([]any); ok {
		return list
	}
	return []any{value}
}

func rewriteSTRMJSONMetadataStreams(streams []any, probe *mediaProbe) (bool, bool) {
	hasVideo := false
	hasAudio := false
	preferredAudio := preferredAudioIndex(*probe)
	audioSelected := false
	for _, item := range streams {
		stream, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch jsonString(jsonObjectValue(stream, "streamType")) {
		case "1":
			hasVideo = true
			setProbeJSONStreamAttributes(stream, probe, "1")
		case "2":
			hasAudio = true
			setProbeJSONStreamAttributes(stream, probe, "2")
			streamIndex, err := strconv.Atoi(jsonString(jsonObjectValue(stream, "index")))
			if err == nil && preferredAudio >= 0 {
				selected := streamIndex == preferredAudio
				setJSONObjectValueAny(stream, "selected", selected)
				audioSelected = audioSelected || selected
			} else if jsonBool(jsonObjectValue(stream, "selected")) {
				audioSelected = true
			}
		}
	}
	return hasVideo, hasAudio
}

func metadataJSONVideoStream(probe mediaProbe) map[string]any {
	for _, stream := range metadataJSONStreams(probe) {
		if object, ok := stream.(map[string]any); ok && jsonString(jsonObjectValue(object, "streamType")) == "1" {
			return object
		}
	}
	return map[string]any{"streamType": 1, "default": true}
}

func metadataJSONAudioStream(probe mediaProbe) map[string]any {
	for _, stream := range metadataJSONStreams(probe) {
		if object, ok := stream.(map[string]any); ok && jsonString(jsonObjectValue(object, "streamType")) == "2" {
			return object
		}
	}
	return map[string]any{"streamType": 2, "selected": true}
}

func metadataJSONStreams(probe mediaProbe) []any {
	if len(probe.Streams) == 0 {
		return []any{metadataJSONVideoStreamFallback(probe), metadataJSONAudioStreamFallback(probe)}
	}
	result := make([]any, 0, len(probe.Streams))
	preferredAudio := preferredAudioIndex(probe)
	for _, stream := range probe.Streams {
		object := map[string]any{"id": syntheticStreamID(stream.Index), "streamType": stream.StreamType}
		if stream.Codec != "" {
			object["codec"] = stream.Codec
		}
		if stream.Index >= 0 {
			object["index"] = stream.Index
		}
		if stream.Language != "" {
			object["language"] = displayLanguage(stream.Language)
			object["languageTag"] = stream.Language
		}
		if stream.Title != "" {
			object["title"] = stream.Title
		}
		if stream.StreamType == 3 {
			object["format"] = stream.Codec
		}
		if stream.Default {
			object["default"] = true
		}
		if stream.Forced {
			object["forced"] = true
		}
		setJSONMetadataStreamAttributes(object, stream)
		if stream.StreamType == 2 && stream.Index == preferredAudio {
			object["selected"] = true
		}
		result = append(result, object)
	}
	return result
}

func syntheticStreamID(index int) int {
	if index < 0 {
		index = 0
	}
	return 900000000 + index
}

func displayLanguage(language string) string {
	switch language {
	case "zh":
		return "中文"
	case "en":
		return "English"
	case "ja":
		return "日本語"
	case "ko":
		return "한국어"
	case "fr":
		return "Français"
	case "de":
		return "Deutsch"
	case "es":
		return "Español"
	default:
		return language
	}
}

func setJSONMetadataStreamAttributes(object map[string]any, stream mediaProbeStream) {
	if stream.Width > 0 {
		object["width"] = stream.Width
	}
	if stream.Height > 0 {
		object["height"] = stream.Height
	}
	if stream.CodedWidth > 0 {
		object["codedWidth"] = stream.CodedWidth
	}
	if stream.CodedHeight > 0 {
		object["codedHeight"] = stream.CodedHeight
	}
	if stream.BitrateKbps > 0 {
		object["bitrate"] = stream.BitrateKbps
	}
	if stream.FrameRate > 0 {
		// Plex's JSON API represents frame rates as formatted strings (for
		// example, "25.000"), even though most other numeric media fields are
		// JSON numbers. The Android client validates this distinction.
		object["frameRate"] = strconv.FormatFloat(stream.FrameRate, 'f', 3, 64)
	}
	if stream.BitDepth > 0 {
		object["bitDepth"] = stream.BitDepth
	}
	if stream.ScanType != "" {
		object["scanType"] = stream.ScanType
	}
	if stream.Channels > 0 {
		object["channels"] = stream.Channels
	}
	if stream.SampleRate > 0 {
		object["samplingRate"] = stream.SampleRate
	}
	if stream.ChannelLayout != "" {
		object["audioChannelLayout"] = stream.ChannelLayout
	}
	if stream.ColorRange != "" {
		object["colorRange"] = stream.ColorRange
	}
	if stream.ColorSpace != "" {
		object["colorSpace"] = stream.ColorSpace
	}
	if stream.ColorTransfer != "" {
		object["colorTransfer"] = stream.ColorTransfer
	}
	if stream.ColorPrimaries != "" {
		object["colorPrimaries"] = stream.ColorPrimaries
	}
}

func metadataJSONVideoStreamFallback(probe mediaProbe) map[string]any {
	stream := map[string]any{"id": syntheticStreamID(0), "streamType": 1, "default": true}
	setProbeJSONStreamAttributes(stream, &probe, "1")
	return stream
}

func metadataJSONAudioStreamFallback(probe mediaProbe) map[string]any {
	stream := map[string]any{"id": syntheticStreamID(1), "streamType": 2, "selected": true}
	setProbeJSONStreamAttributes(stream, &probe, "2")
	return stream
}

func setProbeJSONStreamAttributes(object map[string]any, probe *mediaProbe, streamType string) {
	if probe == nil {
		return
	}
	switch streamType {
	case "1":
		if probe.VideoCodec != "" {
			setJSONObjectValue(object, "codec", probe.VideoCodec)
		}
		if probe.Width > 0 {
			setJSONObjectValueAny(object, "width", probe.Width)
		}
		if probe.Height > 0 {
			setJSONObjectValueAny(object, "height", probe.Height)
		}
		if probe.VideoBitrateKbps > 0 {
			setJSONObjectValueAny(object, "bitrate", probe.VideoBitrateKbps)
		} else if probe.BitrateKbps > 0 {
			setJSONObjectValueAny(object, "bitrate", probe.BitrateKbps)
		}
	case "2":
		if probe.AudioCodec != "" {
			setJSONObjectValue(object, "codec", probe.AudioCodec)
		}
		if probe.AudioChannels > 0 {
			setJSONObjectValueAny(object, "channels", probe.AudioChannels)
		}
		if probe.AudioBitrateKbps > 0 {
			setJSONObjectValueAny(object, "bitrate", probe.AudioBitrateKbps)
		}
	}
}

func setJSONObjectValue(object map[string]any, name, value string) {
	setJSONObjectValueAny(object, name, value)
}

func setJSONObjectValueAny(object map[string]any, name string, value any) {
	for key := range object {
		if strings.EqualFold(key, name) {
			object[key] = value
			return
		}
	}
	object[name] = value
}

func jsonObjectContainsSTRM(object map[string]any) bool {
	for key, value := range object {
		switch strings.ToLower(key) {
		case "file", "path", "key":
			if isSTRMPath(jsonString(value)) {
				return true
			}
		}
	}
	return false
}
