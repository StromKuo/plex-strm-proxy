package app

import (
	"bytes"
	"encoding/xml"
	"io"
	"net/url"
	"strconv"
	"strings"
)

func renderHLSTranscodeDecisionXML(body []byte, contentType string, mapping PartMapping, query url.Values, sessionID string) ([]byte, bool, error) {
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	if mediaType != "application/xml" && mediaType != "text/xml" && mediaType != "application/plex+xml" {
		return nil, false, nil
	}

	width, height := parseVideoResolution(query.Get("videoResolution"))
	if width <= 0 || height <= 0 {
		width, height = 1280, 720
	}
	bitrate := parseBitrateKbps(query.Get("maxVideoBitrate"))

	decoder := xml.NewDecoder(bytes.NewReader(body))
	tokens := make([]xml.Token, 0, 64)
	targetPart := false
	changed := false
	for {
		token, err := decoder.Token()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, false, err
		}
		switch typed := token.(type) {
		case xml.StartElement:
			start := cloneXMLStartElement(typed)
			switch start.Name.Local {
			case "MediaContainer":
				setXMLAttribute(&start, "directPlayDecisionCode", "3000")
				setXMLAttribute(&start, "directPlayDecisionText", "App cannot direct play this item. Direct play is disabled.")
				setXMLAttribute(&start, "generalDecisionCode", "1001")
				setXMLAttribute(&start, "generalDecisionText", "Direct play not available; Conversion OK.")
				setXMLAttribute(&start, "resourceSession", sessionID)
				setXMLAttribute(&start, "transcodeDecisionCode", "1001")
				setXMLAttribute(&start, "transcodeDecisionText", "Direct play not available; Conversion OK.")
			case "Media":
				setXMLAttribute(&start, "bitrate", strconv.Itoa(bitrate))
				setXMLAttribute(&start, "container", "mpegts")
				setXMLAttribute(&start, "protocol", "hls")
				setXMLAttribute(&start, "videoCodec", "h264")
				setXMLAttribute(&start, "audioCodec", "aac")
				setXMLAttribute(&start, "width", strconv.Itoa(width))
				setXMLAttribute(&start, "height", strconv.Itoa(height))
				setXMLAttribute(&start, "selected", "1")
			case "Part":
				part := partRecordFromXMLAttrs(start.Attr)
				targetPart = part.ID == mapping.PartID || xmlAttributesContainSTRM(start.Attr)
				if targetPart {
					removeXMLAttribute(&start, "file")
					removeXMLAttribute(&start, "key")
					setXMLAttribute(&start, "key", hlsPartKey(sessionID))
					setXMLAttribute(&start, "bitrate", strconv.Itoa(bitrate))
					setXMLAttribute(&start, "container", "mpegts")
					setXMLAttribute(&start, "protocol", "hls")
					setXMLAttribute(&start, "decision", "transcode")
					setXMLAttribute(&start, "width", strconv.Itoa(width))
					setXMLAttribute(&start, "height", strconv.Itoa(height))
					setXMLAttribute(&start, "selected", "1")
				}
			}
			tokens = append(tokens, start)
			if start.Name.Local == "Part" && targetPart {
				tokens = append(tokens, hlsVideoStream(width, height), xml.EndElement{Name: xml.Name{Local: "Stream"}}, hlsAudioStream(), xml.EndElement{Name: xml.Name{Local: "Stream"}})
				changed = true
			}
		case xml.EndElement:
			tokens = append(tokens, typed)
			if typed.Name.Local == "Part" {
				targetPart = false
			}
		default:
			tokens = append(tokens, token)
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

func setXMLAttribute(start *xml.StartElement, name, value string) {
	for index := range start.Attr {
		if strings.EqualFold(start.Attr[index].Name.Local, name) {
			start.Attr[index].Value = value
			return
		}
	}
	start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: name}, Value: value})
}

func removeXMLAttribute(start *xml.StartElement, name string) {
	filtered := start.Attr[:0]
	for _, attribute := range start.Attr {
		if !strings.EqualFold(attribute.Name.Local, name) {
			filtered = append(filtered, attribute)
		}
	}
	start.Attr = filtered
}

func hlsVideoStream(width, height int) xml.StartElement {
	return xml.StartElement{Name: xml.Name{Local: "Stream"}, Attr: []xml.Attr{
		{Name: xml.Name{Local: "streamType"}, Value: "1"},
		{Name: xml.Name{Local: "codec"}, Value: "h264"},
		{Name: xml.Name{Local: "width"}, Value: strconv.Itoa(width)},
		{Name: xml.Name{Local: "height"}, Value: strconv.Itoa(height)},
		{Name: xml.Name{Local: "decision"}, Value: "transcode"},
		{Name: xml.Name{Local: "location"}, Value: "segments-av"},
		{Name: xml.Name{Local: "selected"}, Value: "1"},
	}}
}

func hlsAudioStream() xml.StartElement {
	return xml.StartElement{Name: xml.Name{Local: "Stream"}, Attr: []xml.Attr{
		{Name: xml.Name{Local: "streamType"}, Value: "2"},
		{Name: xml.Name{Local: "codec"}, Value: "aac"},
		{Name: xml.Name{Local: "channels"}, Value: "2"},
		{Name: xml.Name{Local: "decision"}, Value: "transcode"},
		{Name: xml.Name{Local: "location"}, Value: "segments-av"},
		{Name: xml.Name{Local: "selected"}, Value: "1"},
	}}
}

func hlsPartKey(sessionID string) string {
	return hlsResourcePrefix + sessionID + "/base/index.m3u8"
}
