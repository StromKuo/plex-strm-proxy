package app

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/url"
	"strings"
)

type decisionDiagnostic struct {
	Root   map[string]string `json:"root"`
	Medias []mediaDiagnostic `json:"medias,omitempty"`
}

type mediaDiagnostic struct {
	Attributes map[string]string `json:"attributes"`
	Parts      []partDiagnostic  `json:"parts,omitempty"`
}

type partDiagnostic struct {
	Attributes  map[string]string   `json:"attributes"`
	FileHost    string              `json:"file_host,omitempty"`
	FilePresent bool                `json:"file_present,omitempty"`
	Streams     []map[string]string `json:"streams,omitempty"`
}

type decisionJSONDiagnostic struct {
	Fields map[string][]string `json:"fields"`
}

var decisionDiagnosticJSONNames = map[string]bool{
	"mdeDecisionCode":        true,
	"mdeDecisionText":        true,
	"directPlayDecisionCode": true,
	"generalDecisionCode":    true,
	"transcodeDecisionCode":  true,
	"directPlayDecisionText": true,
	"generalDecisionText":    true,
	"transcodeDecisionText":  true,
	"protocol":               true,
	"container":              true,
	"videoCodec":             true,
	"audioCodec":             true,
	"videoDecision":          true,
	"audioDecision":          true,
	"decision":               true,
	"location":               true,
	"streamType":             true,
	"width":                  true,
	"height":                 true,
	"duration":               true,
	"bitrate":                true,
	"audioChannels":          true,
	"channels":               true,
	"selected":               true,
	"file":                   true,
}

func summarizeDecisionResponse(contentType string, body []byte) (any, bool) {
	if summary, ok := summarizeDecisionXML(body); ok {
		return summary, true
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return map[string]string{"format": "unparsed", "content_type": contentType, "error": fmt.Sprintf("%T", err)}, false
	}
	result := decisionJSONDiagnostic{Fields: make(map[string][]string)}
	collectDecisionJSONFields(value, "", &result)
	if len(result.Fields) == 0 {
		return result, false
	}
	return result, true
}

func collectDecisionJSONFields(value any, prefix string, result *decisionJSONDiagnostic) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			if decisionDiagnosticJSONNames[key] {
				result.Fields[path] = append(result.Fields[path], diagnosticJSONValue(key, child))
			}
			collectDecisionJSONFields(child, path, result)
		}
	case []any:
		for index, child := range typed {
			collectDecisionJSONFields(child, fmt.Sprintf("%s[%d]", prefix, index), result)
		}
	}
}

func diagnosticJSONValue(key string, value any) string {
	if key == "file" {
		if text, ok := value.(string); ok {
			return decisionURLHost(text)
		}
		return "present"
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}

// summarizeDecisionXML records only playback-relevant attributes. It omits
// signed media URLs and Plex credentials while retaining enough information
// to explain why a client selected direct play or HLS.
func summarizeDecisionXML(body []byte) (decisionDiagnostic, bool) {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	result := decisionDiagnostic{}
	var currentMedia *mediaDiagnostic
	var currentPart *partDiagnostic
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		switch typed := token.(type) {
		case xml.StartElement:
			switch typed.Name.Local {
			case "MediaContainer":
				result.Root = selectedDecisionAttributes(typed.Attr, "directPlayDecisionCode", "generalDecisionCode", "transcodeDecisionCode", "directPlayDecisionText", "generalDecisionText", "transcodeDecisionText")
			case "Media":
				media := mediaDiagnostic{Attributes: selectedDecisionAttributes(typed.Attr,
					"id", "protocol", "container", "videoCodec", "audioCodec", "width", "height", "duration", "bitrate", "videoDecision", "audioDecision", "selected")}
				result.Medias = append(result.Medias, media)
				currentMedia = &result.Medias[len(result.Medias)-1]
			case "Part":
				part := partDiagnostic{Attributes: selectedDecisionAttributes(typed.Attr, "id", "protocol", "container", "decision", "selected", "duration", "size")}
				if file := xmlAttributeValue(typed.Attr, "file"); file != "" {
					part.FilePresent = true
					part.FileHost = decisionURLHost(file)
				}
				if currentMedia != nil {
					currentMedia.Parts = append(currentMedia.Parts, part)
					currentPart = &currentMedia.Parts[len(currentMedia.Parts)-1]
				}
			case "Stream":
				if currentPart != nil {
					currentPart.Streams = append(currentPart.Streams, selectedDecisionAttributes(typed.Attr, "streamType", "codec", "width", "height", "channels", "decision", "location"))
				}
			}
		case xml.EndElement:
			switch typed.Name.Local {
			case "Part":
				currentPart = nil
			case "Media":
				currentMedia = nil
			}
		}
	}
	if result.Root == nil && len(result.Medias) == 0 {
		return decisionDiagnostic{}, false
	}
	return result, true
}

func selectedDecisionAttributes(attributes []xml.Attr, names ...string) map[string]string {
	selected := make(map[string]string, len(names))
	for _, name := range names {
		if value := xmlAttributeValue(attributes, name); value != "" {
			selected[name] = value
		}
	}
	return selected
}

func decisionURLHost(value string) string {
	parsed, err := url.Parse(value)
	if err != nil {
		return "invalid-url"
	}
	if parsed.Hostname() != "" {
		return parsed.Hostname()
	}
	return strings.TrimSpace(parsed.Path)
}
