package app

import (
	"bytes"
	"encoding/xml"
	"io"
	"strconv"
	"strings"
)

// rewriteSTRMDirectDecisionXML fills the missing media part information that
// Plex cannot infer from a .strm file. The decision request has already been
// sent with direct-play enabled; this only makes the resolved source visible
// in the response so clients can select the direct branch.
func rewriteSTRMDirectDecisionXML(body []byte, mapping PartMapping) ([]byte, bool, error) {
	return rewriteSTRMDirectDecisionXMLWithProbe(body, mapping, nil)
}

func rewriteSTRMDirectDecisionXMLWithProbe(body []byte, mapping PartMapping, probe *mediaProbe) ([]byte, bool, error) {
	if mapping.Kind != PartKindSTRM || mapping.PartID == "" || mapping.ResolvedURL == "" {
		return body, false, nil
	}
	container := mediaContainerForURL(mapping.ResolvedURL)
	if container == "" {
		return body, false, nil
	}

	decoder := xml.NewDecoder(bytes.NewReader(body))
	tokens := make([]xml.Token, 0, 64)
	targetPart := false
	partHasVideoStream := false
	partHasAudioStream := false
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
			switch start.Name.Local {
			case "MediaContainer":
				setXMLAttribute(&start, "mdeDecisionCode", "1000")
				setXMLAttribute(&start, "mdeDecisionText", "Direct play is available for the resolved STRM source.")
				setXMLAttribute(&start, "directPlayDecisionCode", "1000")
				setXMLAttribute(&start, "directPlayDecisionText", "Direct play is available for the resolved STRM source.")
				setXMLAttribute(&start, "generalDecisionCode", "1000")
				setXMLAttribute(&start, "generalDecisionText", "Direct play is available for the resolved STRM source.")
				setXMLAttribute(&start, "transcodeDecisionCode", "1000")
				setXMLAttribute(&start, "transcodeDecisionText", "Direct play is available for the resolved STRM source.")
				changed = true
			case "Media":
				setXMLAttribute(&start, "container", container)
				setXMLAttribute(&start, "protocol", "http")
				setXMLAttribute(&start, "videoDecision", "directplay")
				setXMLAttribute(&start, "audioDecision", "directplay")
				setXMLAttribute(&start, "selected", "1")
				if probe != nil {
					setProbeXMLAttributes(&start, *probe, true)
				}
				changed = true
			case "Part":
				part := partRecordFromXMLAttrs(start.Attr)
				targetPart = part.ID == mapping.PartID || xmlAttributesContainSTRM(start.Attr)
				partHasVideoStream = false
				partHasAudioStream = false
				if targetPart {
					setXMLAttribute(&start, "container", container)
					setXMLAttribute(&start, "file", mapping.ResolvedURL)
					setXMLAttribute(&start, "protocol", "http")
					setXMLAttribute(&start, "decision", "directplay")
					setXMLAttribute(&start, "key", directPartKey(mapping))
					setXMLAttribute(&start, "selected", "1")
					changed = true
				}
			case "Stream":
				if targetPart {
					streamType := xmlAttributeValue(start.Attr, "streamType")
					switch streamType {
					case "1":
						partHasVideoStream = true
					case "2":
						partHasAudioStream = true
					}
					setXMLAttribute(&start, "location", "direct")
					selected := true
					if streamType == "2" && probe != nil {
						streamIndex, err := strconv.Atoi(xmlAttributeValue(start.Attr, "index"))
						selected = err == nil && streamIndex == preferredAudioIndex(*probe)
					}
					if selected {
						setXMLAttribute(&start, "selected", "1")
					} else {
						setXMLAttribute(&start, "selected", "0")
					}
					if probe != nil {
						setXMLAttribute(&start, "decision", "directplay")
						setProbeXMLStreamAttributes(&start, *probe, xmlAttributeValue(start.Attr, "streamType"))
					}
					changed = true
				}
			}
			tokens = append(tokens, start)
		case xml.EndElement:
			if typed.Name.Local == "Part" && targetPart && probe != nil {
				for _, stream := range directXMLStreams(*probe, partHasVideoStream, partHasAudioStream) {
					tokens = append(tokens, stream, xml.EndElement{Name: xml.Name{Local: "Stream"}})
					changed = true
				}
			}
			tokens = append(tokens, typed)
			if typed.Name.Local == "Part" {
				targetPart = false
				partHasVideoStream = false
				partHasAudioStream = false
			}
		default:
			tokens = append(tokens, typed)
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

// rewriteSTRMNativeDirectDecisionXML only changes the client-facing source
// URL in a direct decision produced by Plex. All decision codes, stream
// attributes, and unknown fields remain Plex's values. The native decision
// was evaluated against the proxy-local Part; exposing the resolved URL here
// lets the client use the normal direct/302 path without making that local
// URL meaningful on the client device.
func rewriteSTRMNativeDirectDecisionXML(body []byte, mapping PartMapping) ([]byte, bool, error) {
	if mapping.Kind != PartKindSTRM || mapping.PartID == "" || mapping.ResolvedURL == "" {
		return body, false, nil
	}

	decoder := xml.NewDecoder(bytes.NewReader(body))
	tokens := make([]xml.Token, 0, 64)
	changed := false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, false, err
		}
		if start, ok := token.(xml.StartElement); ok && start.Name.Local == "Part" {
			start = cloneXMLStartElement(start)
			part := partRecordFromXMLAttrs(start.Attr)
			if part.ID == mapping.PartID || xmlAttributeValue(start.Attr, "key") == directPartKey(mapping) {
				setXMLAttribute(&start, "file", mapping.ResolvedURL)
				changed = true
			}
			tokens = append(tokens, start)
			continue
		}
		tokens = append(tokens, xml.CopyToken(token))
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

func directPartKey(mapping PartMapping) string {
	if mapping.PartID == "" {
		return mapping.Key
	}
	return "/library/parts/" + mapping.PartID + "/file"
}

func directXMLVideoStream(probe mediaProbe) xml.StartElement {
	start := xml.StartElement{Name: xml.Name{Local: "Stream"}}
	setXMLAttribute(&start, "streamType", "1")
	setXMLAttribute(&start, "location", "direct")
	setXMLAttribute(&start, "selected", "1")
	setXMLAttribute(&start, "decision", "directplay")
	setProbeXMLStreamAttributes(&start, probe, "1")
	return start
}

func directXMLAudioStream(probe mediaProbe) xml.StartElement {
	start := xml.StartElement{Name: xml.Name{Local: "Stream"}}
	setXMLAttribute(&start, "streamType", "2")
	setXMLAttribute(&start, "location", "direct")
	setXMLAttribute(&start, "selected", "1")
	setXMLAttribute(&start, "decision", "directplay")
	setProbeXMLStreamAttributes(&start, probe, "2")
	return start
}

func directXMLStreams(probe mediaProbe, hasVideo, hasAudio bool) []xml.StartElement {
	if len(probe.Streams) == 0 {
		streams := make([]xml.StartElement, 0, 2)
		if !hasVideo {
			streams = append(streams, directXMLVideoStream(probe))
		}
		if !hasAudio {
			streams = append(streams, directXMLAudioStream(probe))
		}
		return streams
	}
	result := make([]xml.StartElement, 0, len(probe.Streams))
	needVideo := !hasVideo
	needAudio := !hasAudio
	preferredAudio := preferredAudioIndex(probe)
	audioSelected := false
	for _, stream := range probe.Streams {
		if stream.StreamType == 1 && !needVideo || stream.StreamType == 2 && !needAudio {
			continue
		}
		start := metadataXMLStream(stream)
		setXMLAttribute(&start, "location", "direct")
		selected := stream.StreamType != 2 || stream.Index == preferredAudio
		if stream.StreamType == 2 && preferredAudio < 0 && !audioSelected {
			selected = true
		}
		if selected {
			setXMLAttribute(&start, "selected", "1")
			audioSelected = audioSelected || stream.StreamType == 2
		} else {
			setXMLAttribute(&start, "selected", "0")
		}
		setXMLAttribute(&start, "decision", "directplay")
		result = append(result, start)
	}
	return result
}

func setProbeXMLAttributes(start *xml.StartElement, probe mediaProbe, media bool) {
	if probe.VideoCodec != "" {
		setXMLAttribute(start, "videoCodec", probe.VideoCodec)
	}
	if probe.AudioCodec != "" {
		setXMLAttribute(start, "audioCodec", probe.AudioCodec)
	}
	if probe.Width > 0 {
		setXMLAttribute(start, "width", strconv.Itoa(probe.Width))
	}
	if probe.Height > 0 {
		setXMLAttribute(start, "height", strconv.Itoa(probe.Height))
	}
	if probe.AudioChannels > 0 {
		setXMLAttribute(start, "audioChannels", strconv.Itoa(probe.AudioChannels))
	}
	if probe.Duration > 0 {
		setXMLAttribute(start, "duration", strconv.FormatInt(int64(probe.Duration*1000), 10))
	}
	if media && probe.BitrateKbps > 0 {
		setXMLAttribute(start, "bitrate", strconv.Itoa(probe.BitrateKbps))
	}
	if probe.VideoFrameRate > 0 {
		setXMLAttribute(start, "videoFrameRate", strconv.FormatFloat(probe.VideoFrameRate, 'f', 3, 64))
	}
	if probe.VideoProfile != "" {
		setXMLAttribute(start, "videoProfile", probe.VideoProfile)
	}
	if probe.AudioProfile != "" {
		setXMLAttribute(start, "audioProfile", probe.AudioProfile)
	}
	if probe.VideoBitDepth > 0 {
		setXMLAttribute(start, "bitDepth", strconv.Itoa(probe.VideoBitDepth))
	}
	if probe.AudioSampleRate > 0 {
		setXMLAttribute(start, "audioSampleRate", strconv.Itoa(probe.AudioSampleRate))
	}
	if probe.ColorRange != "" {
		setXMLAttribute(start, "colorRange", probe.ColorRange)
	}
	if probe.ColorSpace != "" {
		setXMLAttribute(start, "colorSpace", probe.ColorSpace)
	}
	if probe.ColorTransfer != "" {
		setXMLAttribute(start, "colorTransfer", probe.ColorTransfer)
	}
	if probe.ColorPrimaries != "" {
		setXMLAttribute(start, "colorPrimaries", probe.ColorPrimaries)
	}
}

func setProbeXMLStreamAttributes(start *xml.StartElement, probe mediaProbe, streamType string) {
	switch streamType {
	case "1":
		if probe.VideoCodec != "" {
			setXMLAttribute(start, "codec", probe.VideoCodec)
		}
		if probe.Width > 0 {
			setXMLAttribute(start, "width", strconv.Itoa(probe.Width))
		}
		if probe.Height > 0 {
			setXMLAttribute(start, "height", strconv.Itoa(probe.Height))
		}
		if probe.VideoBitrateKbps > 0 {
			setXMLAttribute(start, "bitrate", strconv.Itoa(probe.VideoBitrateKbps))
		} else if probe.BitrateKbps > 0 {
			setXMLAttribute(start, "bitrate", strconv.Itoa(probe.BitrateKbps))
		}
		if probe.VideoFrameRate > 0 {
			setXMLAttribute(start, "frameRate", strconv.FormatFloat(probe.VideoFrameRate, 'f', 3, 64))
		}
		if probe.VideoBitDepth > 0 {
			setXMLAttribute(start, "bitDepth", strconv.Itoa(probe.VideoBitDepth))
		}
		if probe.VideoCodedWidth > 0 {
			setXMLAttribute(start, "codedWidth", strconv.Itoa(probe.VideoCodedWidth))
		}
		if probe.VideoCodedHeight > 0 {
			setXMLAttribute(start, "codedHeight", strconv.Itoa(probe.VideoCodedHeight))
		}
		if probe.VideoScanType != "" {
			setXMLAttribute(start, "scanType", probe.VideoScanType)
		}
		if probe.ColorRange != "" {
			setXMLAttribute(start, "colorRange", probe.ColorRange)
		}
		if probe.ColorSpace != "" {
			setXMLAttribute(start, "colorSpace", probe.ColorSpace)
		}
		if probe.ColorTransfer != "" {
			setXMLAttribute(start, "colorTransfer", probe.ColorTransfer)
		}
		if probe.ColorPrimaries != "" {
			setXMLAttribute(start, "colorPrimaries", probe.ColorPrimaries)
		}
	case "2":
		if probe.AudioCodec != "" {
			setXMLAttribute(start, "codec", probe.AudioCodec)
		}
		if probe.AudioChannels > 0 {
			setXMLAttribute(start, "channels", strconv.Itoa(probe.AudioChannels))
		}
		if probe.AudioBitrateKbps > 0 {
			setXMLAttribute(start, "bitrate", strconv.Itoa(probe.AudioBitrateKbps))
		}
		if probe.AudioSampleRate > 0 {
			setXMLAttribute(start, "samplingRate", strconv.Itoa(probe.AudioSampleRate))
		}
		if probe.AudioChannelLayout != "" {
			setXMLAttribute(start, "audioChannelLayout", probe.AudioChannelLayout)
		}
	}
}

func xmlAttributeValue(attributes []xml.Attr, name string) string {
	for _, attribute := range attributes {
		if strings.EqualFold(attribute.Name.Local, name) {
			return attribute.Value
		}
	}
	return ""
}

func decisionResponsePartMapping(store *MappingStore, contentType string, body []byte) (PartMapping, bool) {
	for _, record := range extractPartRecords(contentType, body) {
		for _, partID := range mappingPartIDs(record) {
			mapping, ok := store.Get(partID)
			if ok && mapping.Kind == PartKindSTRM && mapping.ResolvedURL != "" {
				return mapping, true
			}
		}
	}
	return PartMapping{}, false
}

func isXMLContentType(contentType string) bool {
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	return mediaType == "application/xml" || mediaType == "text/xml" || mediaType == "application/plex+xml"
}
