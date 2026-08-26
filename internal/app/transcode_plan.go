package app

// proxyStreamPlan mirrors Plex's stream-level decision: the container may be
// repackaged while video and audio independently remain copied or are encoded.
// It is used only for STRM fallback responses and does not affect ordinary
// Plex-owned media.
type proxyStreamPlan struct {
	VideoCodec     string
	AudioCodec     string
	VideoDecision  string
	AudioDecision  string
	VideoLocation  string
	AudioLocation  string
	VideoCopy      bool
	AudioTranscode bool
}

func defaultProxyStreamPlan() proxyStreamPlan {
	return proxyStreamPlan{
		VideoCodec:     "h264",
		AudioCodec:     "aac",
		VideoDecision:  "transcode",
		AudioDecision:  "transcode",
		VideoLocation:  "segments-video",
		AudioLocation:  "segments-audio",
		AudioTranscode: true,
	}
}

func proxyStreamPlanFor(profile *ClientPlaybackProfile, probe *mediaProbe) proxyStreamPlan {
	return proxyStreamPlanForMode(profile, probe, false)
}

func proxyStreamPlanForMode(profile *ClientPlaybackProfile, probe *mediaProbe, forceTranscode bool) proxyStreamPlan {
	if forceTranscode {
		return defaultProxyStreamPlan()
	}
	plan := proxyStreamPlan{
		VideoDecision:  "copy",
		AudioCodec:     "aac",
		AudioDecision:  "transcode",
		VideoLocation:  "segments-video",
		AudioLocation:  "segments-audio",
		VideoCopy:      true,
		AudioTranscode: true,
	}
	if probe == nil {
		// Keep the synthetic fallback safe when probing is unavailable. The
		// runtime session still copies video first and falls back to H.264 if
		// that fails, while AAC audio avoids advertising an unknown source audio
		// codec as directly playable.
		return plan
	}

	plan.VideoCodec = probe.VideoCodec
	plan.AudioCodec = probe.AudioCodec
	if sourceVideoNeedsTranscode(profile, probe) {
		plan.VideoCopy = false
		plan.VideoDecision = "transcode"
		plan.VideoCodec = "h264"
	}
	if sourceAudioNeedsTranscode(profile, probe) {
		plan.AudioTranscode = true
		plan.AudioDecision = "transcode"
		plan.AudioCodec = "aac"
	}
	return plan
}
