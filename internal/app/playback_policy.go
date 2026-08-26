package app

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"strings"
)

// PlaybackPlan is the single policy output consumed by request, metadata, and
// decision handling. It describes who owns the next playback step.
type PlaybackPlan string

const (
	PlaybackPlanSTRMRedirect          PlaybackPlan = "strm-redirect"
	PlaybackPlanPlexTranscode         PlaybackPlan = "plex-transcode"
	PlaybackPlanProxyHLSAudioFallback PlaybackPlan = "proxy-hls-audio-fallback"
	PlaybackPlanProxyHLSVideoFallback PlaybackPlan = "proxy-hls-video-fallback"
)

// PlaybackReason makes policy decisions observable without relying on client
// models or duplicating codec checks at each Plex endpoint.
type PlaybackReason string

const (
	PlaybackReasonSTRMDirectFirst      PlaybackReason = "strm-direct-first"
	PlaybackReasonExplicitDirectPlay   PlaybackReason = "explicit-direct-play"
	PlaybackReasonConfirmedDirectPlay  PlaybackReason = "confirmed-direct-play"
	PlaybackReasonAudioCompatibility   PlaybackReason = "audio-compatibility"
	PlaybackReasonVideoCompatibility   PlaybackReason = "video-compatibility"
	PlaybackReasonSTRMHLSCompatibility PlaybackReason = "strm-hls-compatibility"
	PlaybackReasonPlexOwnedMedia       PlaybackReason = "plex-owned-media"
	PlaybackReasonPlexTranscode        PlaybackReason = "plex-transcode"
	PlaybackReasonProxyHLSDisabled     PlaybackReason = "proxy-hls-disabled"
)

type PlaybackStage uint8

const (
	PlaybackStageMetadata PlaybackStage = iota
	PlaybackStageDecisionRequest
	PlaybackStageDecisionResponse
	PlaybackStageTranscodeStart
)

type PlexPlaybackDecision uint8

const (
	PlexDecisionUnknown PlexPlaybackDecision = iota
	PlexDecisionDirectPlay
	PlexDecisionDirectStream
	PlexDecisionTranscode
)

type ClientPlaybackIntent struct {
	DirectPlay   bool
	DirectStream bool
}

type PlaybackPolicyInput struct {
	Stage               PlaybackStage
	IsSTRM              bool
	Probe               *mediaProbe
	ClientProfile       *ClientPlaybackProfile
	PlexDecision        PlexPlaybackDecision
	Client              ClientPlaybackIntent
	HLSTranscodeEnabled bool
	HLSRequest          bool
	DirectPlayConfirmed bool
}

func (s *Server) selectPlaybackPlan(request *http.Request, mapping PartMapping, stage PlaybackStage, decision PlexPlaybackDecision, directConfirmed bool) (PlaybackPolicyResult, *mediaProbe) {
	query := request.URL.Query()
	intent := playbackIntent(query.Get("directPlay"), query.Get("directStream"))
	profile := parseClientPlaybackProfile(request)
	var probe *mediaProbe
	if mapping.Kind == PartKindSTRM && mapping.ResolvedURL != "" && playbackStageAllowsProbe(stage) && !directConfirmed && (!intent.DirectPlay || profile != nil) {
		if value, ok := s.probeSTRMMediaForMapping(request.Context(), mapping); ok {
			probe = &value
		}
	}
	result := SelectPlaybackPlan(PlaybackPolicyInput{
		Stage:               stage,
		IsSTRM:              mapping.Kind == PartKindSTRM,
		Probe:               probe,
		ClientProfile:       profile,
		PlexDecision:        decision,
		Client:              intent,
		HLSTranscodeEnabled: s.hls != nil,
		HLSRequest:          isHLSStartRequest(request),
		DirectPlayConfirmed: directConfirmed,
	})
	return result, probe
}

func playbackStageAllowsProbe(stage PlaybackStage) bool {
	return stage != PlaybackStageMetadata
}

type PlaybackPolicyResult struct {
	Plan   PlaybackPlan
	Reason PlaybackReason
}

// SelectPlaybackPlan is deliberately pure: callers provide all probe,
// decision, intent, and configuration state. Plex remains the owner of normal
// media transcoding; proxy HLS is only selected for an existing STRM fallback.
func SelectPlaybackPlan(input PlaybackPolicyInput) PlaybackPolicyResult {
	if !input.IsSTRM {
		return PlaybackPolicyResult{Plan: PlaybackPlanPlexTranscode, Reason: PlaybackReasonPlexOwnedMedia}
	}
	if input.DirectPlayConfirmed {
		return PlaybackPolicyResult{Plan: PlaybackPlanSTRMRedirect, Reason: PlaybackReasonConfirmedDirectPlay}
	}
	if input.Probe != nil && sourceVideoNeedsTranscode(input.ClientProfile, input.Probe) {
		if input.HLSTranscodeEnabled {
			return PlaybackPolicyResult{Plan: PlaybackPlanProxyHLSVideoFallback, Reason: PlaybackReasonVideoCompatibility}
		}
		return PlaybackPolicyResult{Plan: PlaybackPlanPlexTranscode, Reason: PlaybackReasonPlexTranscode}
	}
	if input.Probe != nil && sourceAudioNeedsTranscode(input.ClientProfile, input.Probe) {
		if input.Client.DirectPlay {
			// An explicit direct-play request is still honored when the client
			// did not provide a usable audio capability list. When a profile is
			// present, sourceAudioNeedsTranscode above has already selected the
			// compatibility path instead.
			if input.ClientProfile == nil || !input.ClientProfile.HasAudio {
				return PlaybackPolicyResult{Plan: PlaybackPlanSTRMRedirect, Reason: PlaybackReasonExplicitDirectPlay}
			}
		}
		if input.HLSTranscodeEnabled {
			return PlaybackPolicyResult{Plan: PlaybackPlanProxyHLSAudioFallback, Reason: PlaybackReasonAudioCompatibility}
		}
		return PlaybackPolicyResult{Plan: PlaybackPlanPlexTranscode, Reason: PlaybackReasonProxyHLSDisabled}
	}
	if input.Client.DirectPlay {
		return PlaybackPolicyResult{Plan: PlaybackPlanSTRMRedirect, Reason: PlaybackReasonExplicitDirectPlay}
	}
	if input.Stage == PlaybackStageTranscodeStart {
		if input.HLSRequest && input.HLSTranscodeEnabled {
			return PlaybackPolicyResult{Plan: PlaybackPlanProxyHLSVideoFallback, Reason: PlaybackReasonSTRMHLSCompatibility}
		}
		return PlaybackPolicyResult{Plan: PlaybackPlanPlexTranscode, Reason: PlaybackReasonPlexTranscode}
	}
	return PlaybackPolicyResult{Plan: PlaybackPlanSTRMRedirect, Reason: PlaybackReasonSTRMDirectFirst}
}

func playbackIntent(requestDirectPlay, requestDirectStream string) ClientPlaybackIntent {
	return ClientPlaybackIntent{
		DirectPlay:   strings.TrimSpace(requestDirectPlay) == "1",
		DirectStream: strings.TrimSpace(requestDirectStream) == "1",
	}
}

func plexDecisionFromBody(contentType string, body []byte) PlexPlaybackDecision {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return PlexDecisionUnknown
	}
	if isJSONContentType(contentType) || trimmed[0] == '{' || trimmed[0] == '[' {
		return plexDecisionFromJSON(trimmed)
	}
	if isXMLContentType(contentType) || trimmed[0] == '<' {
		return plexDecisionFromXML(trimmed)
	}
	return PlexDecisionUnknown
}

type plexDecisionObservation struct {
	decision       PlexPlaybackDecision
	selected       bool
	selectionKnown bool
}

type plexDecisionObservations struct {
	parts     []plexDecisionObservation
	streams   []plexDecisionObservation
	media     []plexDecisionObservation
	container PlexPlaybackDecision
}

type plexSelection struct {
	selected bool
	known    bool
}

func plexDecisionFromXML(body []byte) PlexPlaybackDecision {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	observations := plexDecisionObservations{}
	partSelections := make([]plexSelection, 0, 1)
	for {
		token, err := decoder.Token()
		if err != nil {
			if err == io.EOF {
				return choosePlexDecision(observations)
			}
			return PlexDecisionUnknown
		}
		switch element := token.(type) {
		case xml.StartElement:
			name := strings.ToLower(element.Name.Local)
			switch name {
			case "mediacontainer":
				observations.container = firstKnownDecision(
					observations.container,
					plexDecisionFromXMLAttributes(element.Attr),
				)
			case "part":
				selection := xmlSelection(element.Attr)
				partSelections = append(partSelections, selection)
				if decision := plexDecisionAttribute(element.Attr, "decision"); decision != PlexDecisionUnknown {
					observations.parts = append(observations.parts, plexDecisionObservation{
						decision:       decision,
						selected:       selection.selected,
						selectionKnown: selection.known,
					})
				}
			case "stream":
				if len(partSelections) > 0 {
					if decision := plexDecisionAttribute(element.Attr, "decision"); decision != PlexDecisionUnknown {
						selection := xmlSelection(element.Attr)
						selection = inheritStreamSelection(selection, partSelections[len(partSelections)-1])
						observations.streams = append(observations.streams, plexDecisionObservation{
							decision:       decision,
							selected:       selection.selected,
							selectionKnown: selection.known,
						})
					}
				}
			case "media":
				for _, attribute := range []string{"videoDecision", "audioDecision", "decision"} {
					if decision := plexDecisionAttribute(element.Attr, attribute); decision != PlexDecisionUnknown {
						observations.media = append(observations.media, plexDecisionObservation{decision: decision})
					}
				}
			}
		case xml.EndElement:
			if strings.EqualFold(element.Name.Local, "part") && len(partSelections) > 0 {
				partSelections = partSelections[:len(partSelections)-1]
			}
		}
	}
}

func plexDecisionFromXMLAttributes(attributes []xml.Attr) PlexPlaybackDecision {
	for _, name := range []string{"mdeDecisionCode", "directPlayDecisionCode", "directStreamDecisionCode", "transcodeDecisionCode"} {
		if value := xmlAttributeValue(attributes, name); value != "" {
			if decision := plexDecisionFromCode(value); decision != PlexDecisionUnknown {
				return decision
			}
		}
	}
	return PlexDecisionUnknown
}

func plexDecisionFromJSON(body []byte) PlexPlaybackDecision {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return PlexDecisionUnknown
	}
	observations := plexDecisionObservations{}
	collectJSONDecisionObservations(value, "", plexSelection{}, &observations)
	return choosePlexDecision(observations)
}

func collectJSONDecisionObservations(value any, scope string, partSelection plexSelection, observations *plexDecisionObservations) {
	if observations == nil {
		return
	}
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			collectJSONDecisionObservations(item, scope, partSelection, observations)
		}
	case map[string]any:
		childPartSelection := partSelection
		if scope == "part" {
			childPartSelection = jsonSelection(typed)
			if decision := plexDecisionFromJSONValue(jsonObjectValue(typed, "decision")); decision != PlexDecisionUnknown {
				observations.parts = append(observations.parts, plexDecisionObservation{
					decision:       decision,
					selected:       childPartSelection.selected,
					selectionKnown: childPartSelection.known,
				})
			}
		}
		if scope == "media" {
			for _, name := range []string{"videoDecision", "audioDecision", "decision"} {
				if decision := plexDecisionFromJSONValue(jsonObjectValue(typed, name)); decision != PlexDecisionUnknown {
					observations.media = append(observations.media, plexDecisionObservation{decision: decision})
				}
			}
		}
		if scope == "stream" {
			if decision := plexDecisionFromJSONValue(jsonObjectValue(typed, "decision")); decision != PlexDecisionUnknown {
				selection := inheritStreamSelection(jsonSelection(typed), partSelection)
				observations.streams = append(observations.streams, plexDecisionObservation{
					decision:       decision,
					selected:       selection.selected,
					selectionKnown: selection.known,
				})
			}
		}
		if scope == "container" {
			for _, name := range []string{"mdeDecisionCode", "directPlayDecisionCode", "directStreamDecisionCode", "transcodeDecisionCode"} {
				if decision := plexDecisionFromCode(jsonString(jsonObjectValue(typed, name))); decision != PlexDecisionUnknown {
					observations.container = firstKnownDecision(observations.container, decision)
					break
				}
			}
		}
		for key, child := range typed {
			// Scope applies to the current Part/Media/MediaContainer/Stream object
			// only. A stream decision is collected separately from its Part
			// decision so Plex's stream-level transcode result can be honored.
			childScope := ""
			switch strings.ToLower(key) {
			case "part":
				childScope = "part"
			case "media":
				childScope = "media"
			case "stream":
				childScope = "stream"
			case "mediacontainer":
				childScope = "container"
			}
			collectJSONDecisionObservations(child, childScope, childPartSelection, observations)
		}
	}
}

func xmlSelection(attributes []xml.Attr) plexSelection {
	for _, attribute := range attributes {
		if strings.EqualFold(attribute.Name.Local, "selected") {
			return plexSelection{selected: metadataBool(attribute.Value), known: true}
		}
	}
	return plexSelection{}
}

func jsonSelection(object map[string]any) plexSelection {
	value, ok := objectValue(object, "selected")
	if !ok {
		return plexSelection{}
	}
	return plexSelection{selected: jsonBool(value), known: true}
}

func inheritStreamSelection(stream, part plexSelection) plexSelection {
	if part.known {
		if !stream.known {
			return part
		}
		return plexSelection{selected: stream.selected && part.selected, known: true}
	}
	return stream
}

func plexDecisionAttribute(attributes []xml.Attr, name string) PlexPlaybackDecision {
	return plexDecisionValue(xmlAttributeValue(attributes, name))
}

func plexDecisionFromJSONValue(value any) PlexPlaybackDecision {
	return plexDecisionValue(jsonString(value))
}

func plexDecisionValue(value string) PlexPlaybackDecision {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "directplay":
		return PlexDecisionDirectPlay
	case "directstream":
		return PlexDecisionDirectStream
	case "transcode":
		return PlexDecisionTranscode
	default:
		return PlexDecisionUnknown
	}
}

func plexDecisionFromCode(value string) PlexPlaybackDecision {
	switch strings.TrimSpace(value) {
	case "1000":
		return PlexDecisionDirectPlay
	case "2000":
		return PlexDecisionDirectStream
	case "3000":
		return PlexDecisionTranscode
	default:
		return PlexDecisionUnknown
	}
}

func firstKnownDecision(current, candidate PlexPlaybackDecision) PlexPlaybackDecision {
	if current != PlexDecisionUnknown {
		return current
	}
	return candidate
}

func choosePlexDecision(observations plexDecisionObservations) PlexPlaybackDecision {
	selectedPart := choosePlexDecisionObservations(observations.parts, true)
	selectedStream := choosePlexDecisionObservations(observations.streams, true)
	// Plex can leave Part.decision as directplay while marking the selected
	// video or audio Stream as transcode. The selected stream decision is the
	// authoritative signal for that request and must prevent a stale direct
	// redirect from being remembered.
	if selectedStream == PlexDecisionTranscode || selectedPart == PlexDecisionTranscode {
		return PlexDecisionTranscode
	}
	if selectedPart != PlexDecisionUnknown {
		return selectedPart
	}
	if selectedStream != PlexDecisionUnknown {
		return selectedStream
	}
	allPart := choosePlexDecisionObservations(observations.parts, false)
	allStream := choosePlexDecisionObservations(observations.streams, false)
	if allStream == PlexDecisionTranscode || allPart == PlexDecisionTranscode {
		return PlexDecisionTranscode
	}
	if allPart != PlexDecisionUnknown {
		return allPart
	}
	if allStream != PlexDecisionUnknown {
		return allStream
	}
	if decision := choosePlexDecisionObservations(observations.media, false); decision != PlexDecisionUnknown {
		return decision
	}
	return observations.container
}

func choosePlexDecisionObservations(observations []plexDecisionObservation, selectedOnly bool) PlexPlaybackDecision {
	var hasDirectPlay, hasDirectStream, hasTranscode bool
	for _, observation := range observations {
		if observation.selectionKnown && !observation.selected {
			continue
		}
		if selectedOnly && (!observation.selectionKnown || !observation.selected) {
			continue
		}
		switch observation.decision {
		case PlexDecisionTranscode:
			hasTranscode = true
		case PlexDecisionDirectStream:
			hasDirectStream = true
		case PlexDecisionDirectPlay:
			hasDirectPlay = true
		}
	}
	// A transcode Part must win over a direct-looking Media summary. This is
	// the exact stream-level owner that Plex selected for the requested Part.
	switch {
	case hasTranscode:
		return PlexDecisionTranscode
	case hasDirectStream:
		return PlexDecisionDirectStream
	case hasDirectPlay:
		return PlexDecisionDirectPlay
	default:
		return PlexDecisionUnknown
	}
}
