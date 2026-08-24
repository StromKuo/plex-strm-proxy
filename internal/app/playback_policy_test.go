package app

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"time"
	"testing"
)

func TestSelectPlaybackPlanKeepsDirectFirstAndFallbacksExplicit(t *testing.T) {
	tests := []struct {
		name   string
		input  PlaybackPolicyInput
		want   PlaybackPlan
		reason PlaybackReason
	}{
		{
			name: "strm direct first",
			input: PlaybackPolicyInput{
				IsSTRM:              true,
				HLSTranscodeEnabled: true,
			},
			want:   PlaybackPlanSTRMRedirect,
			reason: PlaybackReasonSTRMDirectFirst,
		},
		{
			name: "explicit direct play wins over audio fallback",
			input: PlaybackPolicyInput{
				IsSTRM:              true,
				Probe:               &mediaProbe{AudioCodec: "pcm_s16le"},
				Client:              ClientPlaybackIntent{DirectPlay: true},
				HLSTranscodeEnabled: true,
			},
			want:   PlaybackPlanSTRMRedirect,
			reason: PlaybackReasonExplicitDirectPlay,
		},
		{
			name: "pcm uses proxy audio fallback",
			input: PlaybackPolicyInput{
				IsSTRM:              true,
				Probe:               &mediaProbe{AudioCodec: "pcm_s16le"},
				HLSTranscodeEnabled: true,
			},
			want:   PlaybackPlanProxyHLSAudioFallback,
			reason: PlaybackReasonAudioCompatibility,
		},
		{
			name: "ordinary Plex transcode stays with Plex",
			input: PlaybackPolicyInput{
				PlexDecision: PlexDecisionTranscode,
			},
			want:   PlaybackPlanPlexTranscode,
			reason: PlaybackReasonPlexOwnedMedia,
		},
		{
			name: "strm HLS video fallback",
			input: PlaybackPolicyInput{
				Stage:               PlaybackStageTranscodeStart,
				IsSTRM:              true,
				HLSRequest:          true,
				HLSTranscodeEnabled: true,
			},
			want:   PlaybackPlanProxyHLSVideoFallback,
			reason: PlaybackReasonSTRMHLSCompatibility,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := SelectPlaybackPlan(test.input)
			if got.Plan != test.want || got.Reason != test.reason {
				t.Fatalf("unexpected playback plan: got=%+v want=%s/%s", got, test.want, test.reason)
			}
		})
	}
}

func TestMediaClientRejectsRedirectToPrivateTarget(t *testing.T) {
	policy := TargetPolicy{
		AllowHTTP:    true,
		AllowHTTPS:   true,
		MaxRedirects: 5,
	}
	client := NewMediaClient(policy)
	redirectTarget, err := url.Parse("http://127.0.0.1:32400/media.mp4")
	if err != nil {
		t.Fatal(err)
	}
	request := &http.Request{URL: redirectTarget}
	if err := client.CheckRedirect(request, nil); err == nil {
		t.Fatal("expected private redirect target to be rejected")
	}
}

func TestHLSSeekStopDoesNotTriggerCopyFallback(t *testing.T) {
	command := exec.Command("sh", "-c", "exit 1")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}

	processDone := make(chan struct{})
	session := &hlsSession{
		id:                "seek-test",
		copyAttempt:       true,
		processStopReason: hlsStopSeek,
		cmd:               command,
	}
	transcoder := &hlsTranscoder{ffmpegPath: "/bin/false", logger: slog.Default()}
	transcoder.waitForProcess(session, command, processDone, new(bytes.Buffer))

	if !session.done || session.cmd != command {
		t.Fatalf("seek stop unexpectedly started a fallback process: done=%v cmd_changed=%v", session.done, session.cmd != command)
	}
	select {
	case <-processDone:
	default:
		t.Fatal("process completion was not signaled")
	}
}

func TestHLSCleanupStopsIdleSession(t *testing.T) {
	command := exec.Command("sh", "-c", "sleep 30")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer command.Process.Kill()

	session := &hlsSession{
		id:         "idle-test",
		directory:  filepath.Join(t.TempDir(), "idle-test"),
		started:    true,
		lastAccess: time.Now().Add(-hlsSessionIdleTimeout - time.Second),
		cmd:        command,
	}
	transcoder := &hlsTranscoder{
		ttl:      time.Hour,
		logger:   slog.Default(),
		sessions: map[string]*hlsSession{session.id: session},
	}

	transcoder.mu.Lock()
	transcoder.cleanupExpiredLocked(time.Now())
	transcoder.mu.Unlock()

	session.mu.Lock()
	stopReason := session.processStopReason
	session.mu.Unlock()
	if stopReason != hlsStopExpired {
		t.Fatalf("idle session was not marked expired: %v", stopReason)
	}
}
