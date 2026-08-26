package app

import (
	"bytes"
	"context"
	"embed"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

//go:embed testdata/plex-native/*.xml
var plexNativeFixtures embed.FS

type plexNativeFixtureCase struct {
	name          string
	file          string
	wantDecision  PlexPlaybackDecision
	strm          bool
	wantDirectURL bool
}

func TestCapturedPlexNativeDecisionFixtures(t *testing.T) {
	tests := []plexNativeFixtureCase{
		{
			name:         "local H264 video with EAC3 audio belongs to Plex",
			file:         "testdata/plex-native/local-h264-eac3.xml",
			wantDecision: PlexDecisionTranscode,
		},
		{
			name:         "local HEVC video with TrueHD audio belongs to Plex",
			file:         "testdata/plex-native/local-hevc-truehd.xml",
			wantDecision: PlexDecisionTranscode,
		},
		{
			name:          "STRM HEVC with selected AAC direct plays",
			file:          "testdata/plex-native/strm-hevc-eac3-aac-direct.xml",
			wantDecision:  PlexDecisionDirectPlay,
			strm:          true,
			wantDirectURL: true,
		},
		{
			name:         "STRM Part directplay with selected stream transcode",
			file:         "testdata/plex-native/strm-hevc-native-stream-transcode.xml",
			wantDecision: PlexDecisionTranscode,
			strm:         true,
		},
		{
			name:         "STRM Part directplay with video copy and audio transcode",
			file:         "testdata/plex-native/strm-hevc-truehd-native-copy.xml",
			wantDecision: PlexDecisionTranscode,
			strm:         true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body, err := plexNativeFixtures.ReadFile(test.file)
			if err != nil {
				t.Fatal(err)
			}
			if got := plexDecisionFromBody("application/xml", body); got != test.wantDecision {
				t.Fatalf("unexpected Plex decision: got=%v want=%v", got, test.wantDecision)
			}

			mapping := PartMapping{
				PartID:      "strm-direct-part",
				Kind:        PartKindSTRM,
				Key:         "/library/parts/strm-direct-part/file",
				ResolvedURL: "https://media.example/fixture-hevc.mkv?sign=fixture",
			}
			if !test.strm {
				return
			}

			if test.wantDirectURL {
				rewritten, changed, err := rewriteSTRMNativeDirectDecisionXML(body, mapping)
				if err != nil {
					t.Fatal(err)
				}
				if !changed || !strings.Contains(string(rewritten), `file="https://media.example/fixture-hevc.mkv?sign=fixture"`) {
					t.Fatalf("native direct result did not expose the resolved source: changed=%v body=%s", changed, rewritten)
				}
				for _, preserved := range []string{`decision="directplay"`, `x-unknown-media="preserve"`, `x-unknown-part="preserve"`} {
					if !strings.Contains(string(rewritten), preserved) {
						t.Fatalf("native direct rewrite dropped Plex field %q: %s", preserved, rewritten)
					}
				}
				return
			}

			// The production path calls the native direct rewriter only after the
			// complete Plex decision is classified. A stream-level transcode must
			// therefore leave the local Part URL untouched.
			if test.wantDecision == PlexDecisionDirectPlay {
				t.Fatalf("fixture must not use direct path for a transcode expectation")
			}
			if plexDecisionFromBody("application/xml", body) == PlexDecisionDirectPlay {
				t.Fatal("stream-level transcode was classified as direct play")
			}
		})
	}
}

func TestCapturedNativeTranscodeDoesNotRememberDirectDecision(t *testing.T) {
	body, err := plexNativeFixtures.ReadFile("testdata/plex-native/strm-hevc-native-stream-transcode.xml")
	if err != nil {
		t.Fatal(err)
	}
	server := newCacheTestServer(t)
	server.mappings.Put(PartMapping{
		PartID:      "strm-transcode-part",
		Kind:        PartKindSTRM,
		STRMPath:    "/media/fixture.strm",
		Key:         "/library/parts/strm-transcode-part/file",
		ResolvedURL: "https://media.example/fixture-hevc.mkv",
	})
	request := httptest.NewRequest(http.MethodGet, "http://proxy.test/video/:/transcode/universal/decision?path=/library/metadata/fixture&session=fixture-transcode&directPlay=0&directStream=0", nil)
	request = request.WithContext(context.WithValue(request.Context(), nativeSTRMKey, true))
	response := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        make(http.Header),
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       request,
	}
	response.Header.Set("Content-Type", "application/xml")
	if err := server.modifyPlexResponse(response); err != nil {
		t.Fatal(err)
	}
	rewritten, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rewritten, body) {
		t.Fatalf("native transcode response was rewritten as a direct response: %s", rewritten)
	}
	if _, ok := server.directDecisionRecord("fixture-transcode"); ok {
		t.Fatal("native stream transcode was incorrectly remembered as a direct decision")
	}
}
