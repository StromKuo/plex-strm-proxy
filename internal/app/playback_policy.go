package app

import (
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
	PlaybackReasonSTRMHLSCompatibility PlaybackReason = "strm-hls-compatibility"
	PlaybackReasonPlexOwnedMedia       PlaybackReason = "plex-owned-media"
	PlaybackReasonPlexTranscode        PlaybackReason = "plex-transcode"
	PlaybackReasonProxyHLSDisabled     PlaybackReason = "proxy-hls-disabled"
)

type PlaybackStage uint8

const (
	PlaybackStageMetadata PlaybackStage = iota
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
	PlexDecision        PlexPlaybackDecision
	Client              ClientPlaybackIntent
	HLSTranscodeEnabled bool
	HLSRequest          bool
	DirectPlayConfirmed bool
}

func (s *Server) selectPlaybackPlan(request *http.Request, mapping PartMapping, stage PlaybackStage, decision PlexPlaybackDecision, directConfirmed bool) (PlaybackPolicyResult, *mediaProbe) {
	query := request.URL.Query()
	intent := playbackIntent(query.Get("directPlay"), query.Get("directStream"))
	var probe *mediaProbe
	if mapping.Kind == PartKindSTRM && mapping.ResolvedURL != "" && !directConfirmed && !intent.DirectPlay {
		if value, ok := s.probeSTRMMedia(request.Context(), mapping.ResolvedURL); ok {
			probe = &value
		}
	}
	result := SelectPlaybackPlan(PlaybackPolicyInput{
		Stage:               stage,
		IsSTRM:              mapping.Kind == PartKindSTRM,
		Probe:               probe,
		PlexDecision:        decision,
		Client:              intent,
		HLSTranscodeEnabled: s.hls != nil,
		HLSRequest:          isHLSStartRequest(request),
		DirectPlayConfirmed: directConfirmed,
	})
	return result, probe
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
	if input.Client.DirectPlay {
		return PlaybackPolicyResult{Plan: PlaybackPlanSTRMRedirect, Reason: PlaybackReasonExplicitDirectPlay}
	}
	if input.Probe != nil && directPlaybackNeedsAudioFallback(*input.Probe) {
		if input.HLSTranscodeEnabled {
			return PlaybackPolicyResult{Plan: PlaybackPlanProxyHLSAudioFallback, Reason: PlaybackReasonAudioCompatibility}
		}
		return PlaybackPolicyResult{Plan: PlaybackPlanPlexTranscode, Reason: PlaybackReasonProxyHLSDisabled}
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
	lower := strings.ToLower(string(body))
	if strings.Contains(strings.ToLower(contentType), "json") {
		switch {
		case strings.Contains(lower, `"decision":"directplay"`), strings.Contains(lower, `"decision": "directplay"`):
			return PlexDecisionDirectPlay
		case strings.Contains(lower, `"decision":"directstream"`), strings.Contains(lower, `"decision": "directstream"`):
			return PlexDecisionDirectStream
		case strings.Contains(lower, `"decision":"transcode"`), strings.Contains(lower, `"decision": "transcode"`):
			return PlexDecisionTranscode
		}
	} else {
		switch {
		case strings.Contains(lower, `decision="directplay"`), strings.Contains(lower, `mdedecisioncode="1000"`):
			return PlexDecisionDirectPlay
		case strings.Contains(lower, `decision="directstream"`):
			return PlexDecisionDirectStream
		case strings.Contains(lower, `decision="transcode"`):
			return PlexDecisionTranscode
		}
	}
	return PlexDecisionUnknown
}
