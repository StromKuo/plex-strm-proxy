package app

import (
	"bytes"
	"compress/gzip"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestResolverParsesBOMAndRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "movie.strm"), []byte("\ufeff\n  \nhttps://media.example/movie.mkv?sig=a%2Bb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "outside.strm"), []byte("https://media.example/outside.mkv"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver([]string{root}, 1024, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	resolved, err := resolver.Resolve(nil, filepath.Join(root, "movie.strm"))
	if err != nil {
		t.Fatal(err)
	}
	if resolved.URL != "https://media.example/movie.mkv?sig=a%2Bb" {
		t.Fatalf("unexpected URL: %q", resolved.URL)
	}

	_, err = resolver.Resolve(nil, filepath.Join(root, "..", filepath.Base(outside), "outside.strm"))
	if !errors.Is(err, ErrPathOutsideRoot) {
		t.Fatalf("expected path traversal rejection, got %v", err)
	}
}

func TestResolverRejectsSymlinkEscapeAndInvalidContent(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "outside.strm")
	if err := os.WriteFile(outsideFile, []byte("https://media.example/outside.mkv"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(root, "link.strm")); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "bad.strm"), []byte("file:///tmp/movie.mkv"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "empty.strm"), []byte("\n  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver([]string{root}, 1024, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	_, err = resolver.Resolve(nil, filepath.Join(root, "link.strm"))
	if !errors.Is(err, ErrPathOutsideRoot) {
		t.Fatalf("expected symlink escape rejection, got %v", err)
	}
	_, err = resolver.Resolve(nil, filepath.Join(root, "bad.strm"))
	if !errors.Is(err, ErrUnsupportedScheme) {
		t.Fatalf("expected unsupported scheme error, got %v", err)
	}
	_, err = resolver.Resolve(nil, filepath.Join(root, "empty.strm"))
	if !errors.Is(err, ErrEmptySTRM) {
		t.Fatalf("expected empty STRM error, got %v", err)
	}
}

func TestMappingStoreExtractsXMLAndJSONParts(t *testing.T) {
	root := t.TempDir()
	strmPath := filepath.Join(root, "movie.strm")
	if err := os.WriteFile(strmPath, []byte("https://media.example/movie.mkv"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver([]string{root}, 1024, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewMappingStore(time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	xmlBody := fmt.Sprintf(`<MediaContainer><Video><Media><Part id="123" key="/library/parts/123/file" file="%s" /></Media></Video></MediaContainer>`, strmPath)
	store.IngestStructuredResponse("application/xml", []byte(xmlBody), "", resolver)
	mapping, ok := store.Get("123")
	if !ok || mapping.Kind != PartKindSTRM || mapping.ResolvedURL != "https://media.example/movie.mkv" {
		t.Fatalf("unexpected XML mapping: %+v, found=%v", mapping, ok)
	}
	if _, ok := store.Get("123"); !ok {
		t.Fatal("expected part ID parsed from Part.key")
	}

	jsonBody := fmt.Sprintf(`{"MediaContainer":{"Media":[{"Part":[{"id":"456","file":"%s"}]}]}}`, strmPath)
	store.IngestStructuredResponse("application/json", []byte(jsonBody), "", resolver)
	mapping, ok = store.Get("456")
	if !ok || mapping.Kind != PartKindSTRM {
		t.Fatalf("unexpected JSON mapping: %+v, found=%v", mapping, ok)
	}

	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, _ = writer.Write([]byte(xmlBody))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeStructuredBody("gzip", compressed.Bytes(), 1<<20)
	if err != nil || string(decoded) != xmlBody {
		t.Fatalf("gzip structured response was not decoded: err=%v", err)
	}
}

func TestSelectMetadataPartByMediaAndPartIndex(t *testing.T) {
	xmlBody := []byte(`<MediaContainer><Video><Media><Part id="101" key="/library/parts/101/file" file="/media/one.strm" /><Part id="102" key="/library/parts/102/file" file="/media/two.mkv" /></Media><Media><Part id="201" key="/library/parts/201/file" file="/media/three.strm" /></Media></Video></MediaContainer>`)
	record, err := selectMetadataPart("application/xml", xmlBody, 1, 0)
	if err != nil || record.ID != "201" {
		t.Fatalf("unexpected XML metadata part: %+v, err=%v", record, err)
	}

	jsonBody := []byte(`{"MediaContainer":{"Metadata":[{"Media":[{"Part":[{"id":"301","key":"/library/parts/301/file","file":"/media/four.strm"}]}]}]}}`)
	record, err = selectMetadataPart("application/json", jsonBody, 0, 0)
	if err != nil || record.ID != "301" {
		t.Fatalf("unexpected JSON metadata part: %+v, err=%v", record, err)
	}
}

func TestRewriteSTRMMetadataAddsPlayableContainers(t *testing.T) {
	xmlBody := []byte(`<MediaContainer><Video><Media><Part id="101" file="/media/movie.strm" /></Media></Video></MediaContainer>`)
	rewritten, changed, err := rewriteSTRMMetadata("application/xml", xmlBody)
	if err != nil || !changed {
		t.Fatalf("expected XML metadata rewrite, changed=%v err=%v", changed, err)
	}
	if got := string(rewritten); !strings.Contains(got, `container="mp4"`) {
		t.Fatalf("rewritten XML does not declare an MP4 container: %s", got)
	}
	var parsedXML struct{}
	if err := xml.Unmarshal(rewritten, &parsedXML); err != nil {
		t.Fatalf("rewritten XML is invalid: %v; body=%s", err, rewritten)
	}

	jsonBody := []byte(`{"MediaContainer":{"Metadata":[{"Media":[{"Part":[{"id":"201","file":"/media/movie.strm"}]}]}]}}`)
	rewritten, changed, err = rewriteSTRMMetadata("application/json", jsonBody)
	if err != nil || !changed {
		t.Fatalf("expected JSON metadata rewrite, changed=%v err=%v", changed, err)
	}
	if got := string(rewritten); !strings.Contains(got, `"container":"mp4"`) || !strings.Contains(got, `"Stream":[]`) {
		t.Fatalf("rewritten JSON does not declare playable metadata: %s", got)
	}

	localBody := []byte(`<MediaContainer><Video><Media><Part id="301" file="/media/movie.mkv" /></Media></Video></MediaContainer>`)
	rewritten, changed, err = rewriteSTRMMetadata("application/xml", localBody)
	if err != nil || changed || !bytes.Equal(rewritten, localBody) {
		t.Fatalf("local metadata should remain unchanged: changed=%v err=%v body=%s", changed, err, rewritten)
	}
}

func TestSTRMMetadataRewriteExposesResolvedFile(t *testing.T) {
	xmlBody := []byte(`<MediaContainer><Video><Media><Part id="321" file="/media/movie.strm" /></Media></Video></MediaContainer>`)
	rewritten, changed, err := rewriteSTRMMetadataWithContainerAndFile("application/xml", xmlBody, func(part partRecord) string {
		return "mkv"
	}, func(part partRecord) string {
		return "https://media.example/movie.mkv?sign=masked"
	})
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("STRM metadata was not rewritten")
	}
	body := string(rewritten)
	for _, expected := range []string{
		`container="mkv"`,
		`file="https://media.example/movie.mkv?sign=masked"`,
		`protocol="http"`,
		`decision="directplay"`,
		`selected="1"`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("resolved STRM metadata is missing %q: %s", expected, body)
		}
	}
}

func TestHubResponseRewritesSTRMMetadata(t *testing.T) {
	root := t.TempDir()
	strmPath := filepath.Join(root, "movie.strm")
	if err := os.WriteFile(strmPath, []byte("https://media.example/movie.mp4\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	plex := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/hubs/continueWatching/items" {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprintf(writer, `<MediaContainer><Video title="Movie"><Media><Part id="321" file=%q /></Media></Video></MediaContainer>`, strmPath)
	}))
	defer plex.Close()

	server := newTestServerWithRoot(t, plex.URL, root, "proxy")
	defer server.Close()
	response, err := server.Client().Get(server.URL + "/hubs/continueWatching/items")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `container="mp4"`) {
		t.Fatalf("hub STRM metadata was not rewritten: status=%d body=%s", response.StatusCode, body)
	}
	if _, ok := server.app.mappings.Get("321"); !ok {
		t.Fatal("hub response did not index the STRM part mapping")
	}
	if cachedProbeCount(server.app) != 0 {
		t.Fatalf("hub metadata unexpectedly probed STRM media: %d cached probes", cachedProbeCount(server.app))
	}
}

func TestPlaybackDecisionProbesSTRMAfterMetadataRewrite(t *testing.T) {
	media := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, "media")
	}))
	defer media.Close()

	tools := t.TempDir()
	ffprobePath := filepath.Join(tools, "ffprobe")
	ffprobeScript := `#!/bin/sh
cat <<'EOF'
{"streams":[{"index":0,"codec_type":"video","codec_name":"h264"},{"index":1,"codec_type":"audio","codec_name":"aac"}],"format":{"duration":"1"}}
EOF
`
	if err := os.WriteFile(ffprobePath, []byte(ffprobeScript), 0o700); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	upstream, err := url.Parse("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.PlexUpstream = upstream
	cfg.STRMRoots = []string{root}
	cfg.AllowPrivate = true
	cfg.AllowedPorts = nil
	cfg.FFmpegPath = filepath.Join(tools, "ffmpeg")
	server, err := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	mapping := PartMapping{
		PartID:      "321",
		Kind:        PartKindSTRM,
		STRMPath:    "/media/movie.strm",
		ResolvedURL: media.URL + "/movie.mp4",
	}
	server.mappings.Put(mapping)
	request := httptest.NewRequest(http.MethodGet, "http://proxy.test/hubs/continueWatching", nil)
	body := []byte(`<MediaContainer><Video><Media><Part id="321" file="/media/movie.strm" /></Media></Video></MediaContainer>`)

	rewritten, changed, err := server.rewriteSTRMMetadataResponse(request, "application/xml", body, false)
	if err != nil || !changed || !strings.Contains(string(rewritten), `container="mp4"`) {
		t.Fatalf("metadata rewrite failed: changed=%v err=%v body=%s", changed, err, rewritten)
	}
	if got := cachedProbeCount(server); got != 0 {
		t.Fatalf("metadata rewrite probed STRM media before playback decision: %d cached probes", got)
	}

	plan, probe := server.selectPlaybackPlan(request, mapping, PlaybackStageDecisionRequest, PlexDecisionUnknown, false)
	if plan.Plan != PlaybackPlanSTRMRedirect || probe == nil || probe.AudioCodec != "aac" {
		t.Fatalf("playback decision did not probe STRM media: plan=%+v probe=%+v", plan, probe)
	}
	if got := cachedProbeCount(server); got != 1 {
		t.Fatalf("expected one cached playback probe, got %d", got)
	}
}

func TestRememberDirectDecisionRejectsIncompatibleProbe(t *testing.T) {
	media := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	defer media.Close()

	tools := t.TempDir()
	ffprobeScript := `#!/bin/sh
cat <<'EOF'
{"streams":[{"codec_type":"video","codec_name":"h264"},{"codec_type":"audio","codec_name":"pcm_s16le"}],"format":{"duration":"1"}}
EOF
`
	if err := os.WriteFile(filepath.Join(tools, "ffprobe"), []byte(ffprobeScript), 0o700); err != nil {
		t.Fatal(err)
	}

	upstream, err := url.Parse("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.PlexUpstream = upstream
	cfg.STRMRoots = []string{t.TempDir()}
	cfg.AllowPrivate = true
	cfg.AllowedPorts = nil
	cfg.FFmpegPath = filepath.Join(tools, "ffmpeg")
	server, err := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	mapping := PartMapping{
		PartID:      "321",
		Kind:        PartKindSTRM,
		STRMPath:    "/media/movie.strm",
		ResolvedURL: media.URL + "/movie.mkv",
	}
	server.mappings.Put(mapping)
	request := httptest.NewRequest(http.MethodGet, "http://proxy.test/video/:/transcode/universal/decision?session=audio-fallback&directPlay=0&directStream=0", nil)
	body := []byte(`<MediaContainer><Video><Media><Part id="321" file="/media/movie.strm" decision="directplay" /></Media></Video></MediaContainer>`)

	server.rememberDirectDecision(request, "application/xml", body)
	if _, ok := server.directDecisionRecord("audio-fallback"); ok {
		t.Fatal("incompatible directplay response was recorded as a direct decision")
	}
	if got := cachedProbeCount(server); got != 1 {
		t.Fatalf("expected the decision response to reuse one playback probe, got %d", got)
	}
}

func TestDirectDecisionCachesCompatibleRetry(t *testing.T) {
	media := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "video/x-matroska")
		writer.Header().Set("Content-Length", "5")
		writer.WriteHeader(http.StatusOK)
		if request.Method != http.MethodHead {
			_, _ = io.WriteString(writer, "media")
		}
	}))
	defer media.Close()

	tools := t.TempDir()
	if err := os.WriteFile(filepath.Join(tools, "ffmpeg"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ffprobe := `#!/bin/sh
sleep 1
cat <<'EOF'
{"streams":[{"index":0,"codec_type":"video","codec_name":"h264"},{"index":1,"codec_type":"audio","codec_name":"aac"}],"format":{"duration":"1"}}
EOF
`
	if err := os.WriteFile(filepath.Join(tools, "ffprobe"), []byte(ffprobe), 0o700); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	strmPath := filepath.Join(root, "movie.strm")
	if err := os.WriteFile(strmPath, []byte(media.URL+"/movie.mkv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plex := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/library/metadata/42":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(writer, `{"MediaContainer":{"Metadata":[{"Media":[{"Part":[{"id":"321","key":"/library/parts/321/file","file":%q}]}]}]}}`, strmPath)
		case "/video/:/transcode/universal/decision":
			writer.Header().Set("Content-Type", "application/xml")
			_, _ = fmt.Fprintf(writer, `<MediaContainer><Video><Media><Part id="321" file=%q decision="directplay" /></Media></Video></MediaContainer>`, strmPath)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer plex.Close()

	upstream, err := url.Parse(plex.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.PlexUpstream = upstream
	cfg.STRMRoots = []string{root}
	cfg.AllowPrivate = true
	cfg.AllowedPorts = nil
	cfg.FFmpegPath = filepath.Join(tools, "ffmpeg")
	app, err := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	app.mappings.Put(PartMapping{
		PartID:      "321",
		Kind:        PartKindSTRM,
		STRMPath:    "/media/movie.strm",
		ResolvedURL: media.URL + "/movie.mkv",
	})
	server := httptest.NewServer(app.Handler())
	defer server.Close()

	decisionPath := url.QueryEscape("/library/metadata/42")
	decisionURL := server.URL + "/video/:/transcode/universal/decision?path=" + decisionPath + "&session=compatible-retry&directPlay=1&directStream=0"
	started := time.Now()
	response, err := server.Client().Get(decisionURL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if elapsed := time.Since(started); elapsed >= 900*time.Millisecond {
		t.Fatalf("first direct decision waited for the probe: %s", elapsed)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `decision="directplay"`) {
		t.Fatalf("unexpected first direct decision: status=%d headers=%v body=%s", response.StatusCode, response.Header, body)
	}
	if decision, ok := app.directDecisionRecord("compatible-retry"); !ok {
		t.Fatalf("first direct decision was not cached: probes=%d mappings=%+v", cachedProbeCount(app), app.mappings)
	} else if decision.mapping.PartID != "321" || len(decision.body) == 0 {
		t.Fatalf("cached direct decision is incomplete: %+v", decision)
	}

	secondURL := server.URL + "/video/:/transcode/universal/decision?path=" + decisionPath + "&session=compatible-retry&directPlay=0&directStream=0"
	response, err = server.Client().Get(secondURL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("X-Plex-Strm-Proxy") != "direct-cache" {
		body, _ = io.ReadAll(response.Body)
		t.Fatalf("compatible retry did not stay direct: status=%d headers=%v body=%s", response.StatusCode, response.Header, body)
	}
}

func cachedProbeCount(server *Server) int {
	server.probeMu.Lock()
	defer server.probeMu.Unlock()
	return len(server.probes)
}

func TestDetailedMetadataResponseDoesNotProbeSTRMMedia(t *testing.T) {
	media := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, "media")
	}))
	defer media.Close()

	tools := t.TempDir()
	if err := os.WriteFile(filepath.Join(tools, "ffprobe"), []byte("#!/bin/sh\necho '{\"streams\":[],\"format\":{}}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	strmPath := filepath.Join(root, "movie.strm")
	if err := os.WriteFile(strmPath, []byte(media.URL+"/movie.mp4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plex := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/library/metadata/42" {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprintf(writer, `<MediaContainer><Video><Media><Part id="321" file=%q /></Media></Video></MediaContainer>`, strmPath)
	}))
	defer plex.Close()

	server := newTestServerWithRoot(t, plex.URL, root, "redirect")
	defer server.Close()
	server.app.cfg.FFmpegPath = filepath.Join(tools, "ffmpeg")

	response, err := server.Client().Get(server.URL + "/library/metadata/42")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `container="mp4"`) {
		t.Fatalf("unexpected metadata response: status=%d body=%s", response.StatusCode, body)
	}
	if got := cachedProbeCount(server.app); got != 0 {
		t.Fatalf("detailed metadata unexpectedly probed STRM media: %d cached probes", got)
	}
}

func TestTransparentProxyPreservesPlexTokenAndPath(t *testing.T) {
	plex := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/library/metadata/1" || request.URL.Query().Get("foo") != "bar" {
			t.Errorf("unexpected upstream URL: %s", request.URL.String())
		}
		if request.Header.Get("X-Plex-Token") != "secret" {
			t.Errorf("Plex token was not forwarded upstream")
		}
		writer.Header().Set("Content-Type", "text/plain")
		writer.WriteHeader(http.StatusAccepted)
		_, _ = writer.Write([]byte("plex-response"))
	}))
	defer plex.Close()

	server := newTestServer(t, plex.URL, "redirect")
	defer server.Close()
	request, err := http.NewRequest(http.MethodGet, server.URL+"/library/metadata/1?foo=bar", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Plex-Token", "secret")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusAccepted || string(body) != "plex-response" {
		t.Fatalf("unexpected response: %d %q", response.StatusCode, body)
	}
}

func TestTransparentProxyRewritesPlexUpstreamLocation(t *testing.T) {
	var plex *httptest.Server
	plex = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Host == "" || request.Host == strings.TrimPrefix(plex.URL, "http://") {
			t.Errorf("proxy did not preserve the public Host: %q", request.Host)
		}
		writer.Header().Set("Location", plex.URL+"/web/index.html")
		writer.WriteHeader(http.StatusFound)
	}))
	defer plex.Close()

	server := newTestServer(t, plex.URL, "redirect")
	defer server.Close()
	client := server.Client()
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Get(server.URL + "/web")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusFound {
		t.Fatalf("expected 302, got %d", response.StatusCode)
	}
	if got := response.Header.Get("Location"); got != "/web/index.html" {
		t.Fatalf("unexpected rewritten location: %q", got)
	}
}

func TestMetadataDecisionEnablesDirectPlaybackForSTRM(t *testing.T) {
	root := t.TempDir()
	strmPath := filepath.Join(root, "movie.strm")
	if err := os.WriteFile(strmPath, []byte("https://media.example/movie.mkv\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	plex := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/library/metadata/42":
			if request.Header.Get("X-Plex-Token") != "secret" {
				t.Errorf("metadata lookup did not forward Plex token")
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(writer, `{"MediaContainer":{"Metadata":[{"Media":[{"Part":[{"id":"321","key":"/library/parts/321/file","file":%q}]}]}]}}`, strmPath)
		case "/video/:/transcode/universal/decision":
			if got := request.URL.Query().Get("path"); got != "/library/metadata/42" {
				t.Errorf("metadata path was unexpectedly rewritten: %q", got)
			}
			if got := request.URL.Query().Get("protocol"); got != "" {
				t.Errorf("client protocol should be preserved: %q", got)
			}
			if request.URL.Query().Get("directPlay") != "1" || request.URL.Query().Get("directStream") != "0" {
				t.Errorf("direct playback was not enabled: %s", request.URL.RawQuery)
			}
			writer.Header().Set("Content-Type", "application/xml")
			_, _ = fmt.Fprintf(writer, `<MediaContainer><Video><Media><Part id="321" key="/library/parts/321/file" file=%q /></Media></Video></MediaContainer>`, strmPath)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer plex.Close()

	server := newTestServerWithRoot(t, plex.URL, root, "proxy")
	defer server.Close()
	request, err := http.NewRequest(http.MethodGet, server.URL+"/video/:/transcode/universal/decision?path="+url.QueryEscape("/library/metadata/42")+"&mediaIndex=0&partIndex=0&directPlay=0&directStream=0", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Plex-Token", "secret")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("unexpected decision response: %d", response.StatusCode)
	}
	if !strings.Contains(string(body), `container="mkv"`) {
		t.Fatalf("decision response did not declare the resolved STRM container: %s", body)
	}
	mapping, ok := server.app.mappings.Get("321")
	if !ok || mapping.Kind != PartKindSTRM || mapping.ResolvedURL != "https://media.example/movie.mkv" {
		t.Fatalf("metadata STRM mapping was not indexed: %+v, found=%v", mapping, ok)
	}
}

func TestTranscodeStartEnablesDirectPlaybackForSTRM(t *testing.T) {
	root := t.TempDir()
	strmPath := filepath.Join(root, "movie.strm")
	if err := os.WriteFile(strmPath, []byte("https://media.example/movie.mp4\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	plex := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/library/metadata/42":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(writer, `{"MediaContainer":{"Metadata":[{"Media":[{"Part":[{"id":"321","file":%q}]}]}]}}`, strmPath)
		case "/video/:/transcode/universal/start.mpd":
			if got := request.URL.Query().Get("path"); got != "https://media.example/movie.mp4" {
				http.Error(writer, "resolved media path was not forwarded", http.StatusBadRequest)
				return
			}
			if request.URL.Query().Get("directPlay") != "1" || request.URL.Query().Get("directStream") != "0" {
				http.Error(writer, "direct playback was not enabled", http.StatusBadRequest)
				return
			}
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write([]byte("start-ok"))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer plex.Close()

	server := newTestServerWithRoot(t, plex.URL, root, "proxy")
	defer server.Close()
	request, err := http.NewRequest(http.MethodGet, server.URL+"/video/:/transcode/universal/start.mpd?path="+url.QueryEscape("/library/metadata/42")+"&mediaIndex=0&partIndex=0&directPlay=0&directStream=0", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || string(body) != "start-ok" {
		t.Fatalf("unexpected transcode start response: %d %q", response.StatusCode, body)
	}
}

func TestHLSPlaybackIsAllowedToTranscode(t *testing.T) {
	request, err := http.NewRequest(http.MethodGet, "http://proxy.test/video/:/transcode/universal/decision?protocol=hls&directPlay=1&directStream=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	query := prepareSTRMPlaybackQuery(request)
	if !isHLSPlayback(query) {
		t.Fatal("expected HLS protocol to be detected")
	}
	if query.Get("directPlay") != "0" || query.Get("directStream") != "0" {
		t.Fatalf("HLS playback flags were not set for Plex transcoding: %s", query.Encode())
	}
}

func TestDirectPlaybackQueryUsesResolvedSource(t *testing.T) {
	request, err := http.NewRequest(http.MethodGet, "http://proxy.test/video/:/transcode/universal/decision?protocol=hls&directPlay=0&directStream=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	query := prepareSTRMDirectPlaybackQuery(request, "https://media.example/movie.mkv")
	if query.Get("path") != "https://media.example/movie.mkv" {
		t.Fatalf("resolved source path was not set: %s", query.Encode())
	}
	if query.Get("protocol") != "http" {
		t.Fatalf("direct-first decision should use generic HTTP transport: %s", query.Encode())
	}
	if query.Get("directPlay") != "1" || query.Get("directStream") != "0" {
		t.Fatalf("direct-first playback flags were not set: %s", query.Encode())
	}
}

func TestPreferredAudioSelectsAACTrack(t *testing.T) {
	probe := mediaProbe{
		AudioCodec:    "eac3",
		AudioChannels: 6,
		Streams: []mediaProbeStream{
			{StreamType: 1, Index: 0, Codec: "hevc"},
			{StreamType: 2, Index: 1, Codec: "eac3", Channels: 6, Language: "zh", Default: true},
			{StreamType: 2, Index: 2, Codec: "aac", Channels: 2, Language: "zh"},
		},
	}

	selectPreferredAudio(&probe)
	if probe.PreferredAudioIndex != 2 || probe.AudioCodec != "aac" || probe.AudioChannels != 2 {
		t.Fatalf("AAC track was not preferred: %+v", probe)
	}

	streams := metadataJSONStreams(probe)
	selected := make([]int, 0, 2)
	for _, item := range streams {
		stream, ok := item.(map[string]any)
		if !ok || jsonString(jsonObjectValue(stream, "streamType")) != "2" || !jsonBool(jsonObjectValue(stream, "selected")) {
			continue
		}
		index, err := strconv.Atoi(jsonString(jsonObjectValue(stream, "index")))
		if err != nil {
			t.Fatalf("selected audio stream has invalid index: %v", err)
		}
		selected = append(selected, index)
	}
	if !reflect.DeepEqual(selected, []int{2}) {
		t.Fatalf("unexpected selected audio streams: %v", selected)
	}
}

func TestDirectPlaybackAudioFallbackIsCodecBased(t *testing.T) {
	if directPlaybackNeedsAudioFallback(mediaProbe{AudioCodec: "aac"}) {
		t.Fatal("AAC should remain eligible for direct playback")
	}
	if !directPlaybackNeedsAudioFallback(mediaProbe{AudioCodec: "eac3"}) {
		t.Fatal("E-AC-3 should use the compatibility fallback by default")
	}
	for _, codec := range []string{"pcm_s16le", "pcm_s24le", "pcm_s32be", "pcm_f32le"} {
		if !directPlaybackNeedsAudioFallback(mediaProbe{AudioCodec: codec}) {
			t.Fatalf("%s should use the compatibility fallback by default", codec)
		}
	}

	session := &hlsSession{
		sourceURL:      "https://media.example/movie.mp4",
		directory:      t.TempDir(),
		bitrateKbps:    4000,
		copyAttempt:    true,
		audioTranscode: true,
	}
	args := strings.Join(buildFFmpegArgs(session), " ")
	if !strings.Contains(args, "-c:v copy") || !strings.Contains(args, "-c:a aac") {
		t.Fatalf("expected video copy plus AAC audio transcode: %s", args)
	}
	if strings.Contains(args, "-c:a copy") {
		t.Fatalf("audio should not be copied in the compatibility fallback: %s", args)
	}
}

func TestRewriteSTRMDirectDecisionXML(t *testing.T) {
	body := []byte(`<MediaContainer><Video><Media container="mkv" protocol="hls"><Part id="321" file="/media/movie.strm" protocol="hls" decision="transcode"><Stream streamType="1" decision="transcode" /></Part></Media></Video></MediaContainer>`)
	probe := &mediaProbe{Width: 1920, Height: 1080, VideoCodec: "h264", AudioCodec: "aac", AudioChannels: 2, BitrateKbps: 5046}
	rewritten, changed, err := rewriteSTRMDirectDecisionXMLWithProbe(body, PartMapping{
		PartID:      "321",
		Kind:        PartKindSTRM,
		ResolvedURL: "https://media.example/movie.mkv",
	}, probe)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("STRM decision was not rewritten")
	}
	text := string(rewritten)
	for _, expected := range []string{
		`mdeDecisionCode="1000"`,
		`file="https://media.example/movie.mkv"`,
		`container="mkv"`,
		`decision="directplay"`,
		`streamType="1"`,
		`streamType="2"`,
		`location="direct"`,
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("rewritten decision is missing %q: %s", expected, text)
		}
	}
}

func TestRewriteSTRMDirectDecisionJSON(t *testing.T) {
	body := []byte(`{"MediaContainer":{"Metadata":[{"Media":[{"protocol":"hls","container":"strm","Part":[{"id":"321","file":"/media/movie.strm","protocol":"hls","decision":"transcode","selected":false,"Stream":[{"streamType":1,"decision":"transcode"}]}]}]}]}}`)
	probe := &mediaProbe{Duration: 123.5, BitrateKbps: 8192, Width: 3840, Height: 2160, VideoCodec: "hevc", AudioCodec: "eac3", AudioChannels: 6}
	rewritten, changed, err := rewriteSTRMDirectDecisionJSONWithProbe(body, PartMapping{
		PartID:      "321",
		Kind:        PartKindSTRM,
		ResolvedURL: "https://media.example/movie.mkv",
	}, probe)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("STRM JSON decision was not rewritten")
	}
	text := string(rewritten)
	for _, expected := range []string{
		`"mdeDecisionCode":1000`,
		`"file":"https://media.example/movie.mkv"`,
		`"container":"mkv"`,
		`"decision":"directplay"`,
		`"location":"direct"`,
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("rewritten JSON decision is missing %q: %s", expected, text)
		}
	}
}

func TestRenderHLSTranscodeDecisionJSON(t *testing.T) {
	body := []byte(`{"MediaContainer":{"Metadata":[{"Media":[{"Part":[{"id":321,"file":"/OpenListSTRM/movie.strm"}]}]}]}}`)
	rewritten, changed, err := renderHLSTranscodeDecisionJSON(body, PartMapping{PartID: "321", Kind: PartKindSTRM}, url.Values{"videoResolution": {"1280x720"}, "maxVideoBitrate": {"2000"}}, "mobile-test")
	if err != nil || !changed {
		t.Fatalf("JSON HLS decision was not rewritten: changed=%v err=%v", changed, err)
	}
	text := string(rewritten)
	for _, expected := range []string{`"protocol":"hls"`, `"container":"mpegts"`, `"codec":"h264"`, `"codec":"aac"`, `"decision":"transcode"`, `/video/:/transcode/universal/session/mobile-test/base/index.m3u8`} {
		if !strings.Contains(text, expected) {
			t.Fatalf("rewritten JSON HLS decision is missing %q: %s", expected, text)
		}
	}
}

func TestDASHResourcesAreNotClaimedByProxyHLS(t *testing.T) {
	for _, relative := range []string{"base/index.m3u8", "base/00000.ts", "seek-00012.m3u8"} {
		if !isProxyOwnedHLSResource(relative) {
			t.Fatalf("proxy HLS resource %q was not recognized", relative)
		}
	}
	for _, relative := range []string{"0/header", "1/header", "0/init-stream0.m4s", "1/chunk-stream1-00001.m4s"} {
		if isProxyOwnedHLSResource(relative) {
			t.Fatalf("DASH resource %q was incorrectly claimed by proxy HLS", relative)
		}
	}
}

func TestWriteHLSVODPlaylistUsesWholeDuration(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "index.m3u8")
	if err := writeHLSVODPlaylist(filename, 12.25); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	playlist := string(body)
	for _, expected := range []string{
		"#EXT-X-PLAYLIST-TYPE:VOD",
		"#EXTINF:5.000,\n00000.ts",
		"#EXTINF:5.000,\n00001.ts",
		"#EXTINF:2.250,\n00002.ts",
		"#EXT-X-ENDLIST",
	} {
		if !strings.Contains(playlist, expected) {
			t.Fatalf("VOD playlist is missing %q: %s", expected, playlist)
		}
	}
}

func TestParseHLSStartOffset(t *testing.T) {
	if got := parseHLSStartOffset(url.Values{"offset": {"264000"}}); got != 264 {
		t.Fatalf("unexpected HLS start offset: %f", got)
	}
	if got := parseHLSStartOffset(url.Values{"offset": {"invalid"}}); got != 0 {
		t.Fatalf("invalid HLS start offset should be ignored: %f", got)
	}
}

func TestBuildFFmpegArgsForHLSSeek(t *testing.T) {
	session := &hlsSession{
		sourceURL:   "https://media.example/movie.mkv",
		directory:   "/tmp/session",
		width:       1280,
		height:      720,
		bitrateKbps: 2000,
	}
	args := strings.Join(buildFFmpegArgsForRange(session, 260, 52, "seek-00052.m3u8", hlsSeekWindowSeconds), " ")
	for _, expected := range []string{"-ss 260.000", "-seekable 0", "-start_number 52", "-t 60.000", "seek-00052.m3u8"} {
		if !strings.Contains(args, expected) {
			t.Fatalf("HLS seek arguments are missing %q: %s", expected, args)
		}
	}
}

func TestBuildFFmpegArgsUsesStreamCopyWhenRequested(t *testing.T) {
	session := &hlsSession{
		sourceURL:   "https://media.example/movie.mkv",
		directory:   "/tmp/session",
		bitrateKbps: 2000,
		copyAttempt: true,
	}
	args := strings.Join(buildFFmpegArgs(session), " ")
	if !strings.Contains(args, "-c:v copy") || !strings.Contains(args, "-c:a copy") {
		t.Fatalf("stream-copy arguments are missing: %s", args)
	}
	if strings.Contains(args, "libx264") || strings.Contains(args, "-vf") {
		t.Fatalf("stream-copy arguments unexpectedly transcode video: %s", args)
	}
}

func TestSTRMDecisionProvidesResolvedDirectPart(t *testing.T) {
	root := t.TempDir()
	strmPath := filepath.Join(root, "movie.strm")
	if err := os.WriteFile(strmPath, []byte("https://media.example/movie.mkv\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	plex := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/library/metadata/42" {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(writer, `{"MediaContainer":{"Metadata":[{"Media":[{"Part":[{"id":"321","key":"/library/parts/321/file","file":%q}]}]}]}}`, strmPath)
			return
		}
		if request.URL.Path != "/video/:/transcode/universal/decision" {
			http.NotFound(writer, request)
			return
		}
		if request.URL.Query().Get("path") != "/library/metadata/42" {
			t.Errorf("metadata path was unexpectedly rewritten: %q", request.URL.Query().Get("path"))
		}
		if request.URL.Query().Get("protocol") != "http" || request.URL.Query().Get("directPlay") != "1" {
			t.Errorf("direct-first flags were not forwarded: %s", request.URL.RawQuery)
		}
		writer.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(writer, `<MediaContainer generalDecisionCode="1001"><Video><Media protocol="hls"><Part id="321" decision="transcode" /></Media></Video></MediaContainer>`)
	}))
	defer plex.Close()

	server := newTestServerWithRoot(t, plex.URL, root, "proxy")
	defer server.Close()
	request, err := http.NewRequest(http.MethodGet, server.URL+"/video/:/transcode/universal/decision?path="+url.QueryEscape("/library/metadata/42")+"&protocol=hls&session=mobile-test&directPlay=1&directStream=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || response.Header.Get("X-Plex-Strm-Proxy") == "hls-transcode" {
		t.Fatalf("decision should be returned by Plex before fallback: status=%d headers=%v body=%s", response.StatusCode, response.Header, body)
	}
	if !strings.Contains(string(body), `decision="directplay"`) {
		t.Fatalf("resolved direct decision was not exposed: %s", body)
	}
	if !strings.Contains(string(body), `protocol="http"`) {
		t.Fatalf("direct decision did not expose HTTP Part protocol: %s", body)
	}
	if !strings.Contains(string(body), `directPlayDecisionCode="1000"`) {
		t.Fatalf("direct decision root code was not rewritten: %s", body)
	}
	if !strings.Contains(string(body), `key="/library/parts/321/file"`) {
		t.Fatalf("direct decision did not point Part key to the proxy endpoint: %s", body)
	}
	if _, ok := server.app.hls.session("mobile-test"); ok {
		t.Fatal("HLS fallback should not start until Plex requests the transcode start endpoint")
	}
}

func TestDirectDecisionHLSStartRedirectsToResolvedSource(t *testing.T) {
	media := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	defer media.Close()

	root := t.TempDir()
	strmPath := filepath.Join(root, "movie.strm")
	if err := os.WriteFile(strmPath, []byte(media.URL+"/movie.mkv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plex := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/library/metadata/42":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(writer, `{"MediaContainer":{"Metadata":[{"Media":[{"Part":[{"id":"321","key":"/library/parts/321/file","file":%q}]}]}]}}`, strmPath)
		case "/video/:/transcode/universal/decision":
			writer.Header().Set("Content-Type", "application/xml")
			_, _ = fmt.Fprintf(writer, `<MediaContainer mdeDecisionCode="1000"><Video><Media><Part id="321" key="/library/parts/321/file" file=%q decision="directplay" /></Media></Video></MediaContainer>`, strmPath)
		case "/video/:/transcode/universal/start.m3u8":
			t.Errorf("direct decision should not start HLS fallback")
			http.Error(writer, "unexpected HLS fallback", http.StatusInternalServerError)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer plex.Close()

	server := newTestServerWithRoot(t, plex.URL, root, "redirect")
	defer server.Close()
	client := server.Client()
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	decisionURL := server.URL + "/video/:/transcode/universal/decision?path=" + url.QueryEscape("/library/metadata/42") + "&protocol=hls&session=direct-test&mediaIndex=0&partIndex=0"
	response, err := client.Get(decisionURL)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(response.Body)
	response.Body.Close()

	startURL := server.URL + "/video/:/transcode/universal/start.m3u8?path=" + url.QueryEscape("/library/metadata/42") + "&protocol=hls&session=direct-test&directPlay=0&directStream=1&mediaIndex=0&partIndex=0"
	response, err = client.Get(startURL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusFound {
		t.Fatalf("expected direct redirect, got %d", response.StatusCode)
	}
	if response.Header.Get("Location") != media.URL+"/movie.mkv" {
		t.Fatalf("unexpected direct location: %q", response.Header.Get("Location"))
	}
}

func TestDecisionRewritesPathAndIndexesPartForRedirect(t *testing.T) {
	media := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("media"))
	}))
	defer media.Close()

	root := t.TempDir()
	strmPath := filepath.Join(root, "movie.strm")
	if err := os.WriteFile(strmPath, []byte(media.URL+"/movie.mkv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plex := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.URL.Query().Get("path"); got != media.URL+"/movie.mkv" {
			t.Errorf("decision path was not rewritten: %q", got)
		}
		writer.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprintf(writer, `<MediaContainer><Media><Part id="123" file="%s" key="/library/parts/123/file" /></Media></MediaContainer>`, strmPath)
	}))
	defer plex.Close()

	server := newTestServerWithRoot(t, plex.URL, root, "redirect")
	defer server.Close()
	client := server.Client()
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	requestURL := server.URL + "/video/:/transcode/universal/decision?path=" + url.QueryEscape(strmPath)
	response, err := client.Get(requestURL)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(response.Body)
	response.Body.Close()

	response, err = client.Get(server.URL + "/library/parts/123/1786724686/file")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusFound {
		t.Fatalf("expected 302, got %d", response.StatusCode)
	}
	if response.Header.Get("Location") != media.URL+"/movie.mkv" {
		t.Fatalf("unexpected redirect location: %q", response.Header.Get("Location"))
	}
}

func TestMediaProxySupportsRangeAndDoesNotForwardPlexToken(t *testing.T) {
	media := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Plex-Token") != "" || request.Header.Get("Authorization") != "" {
			t.Errorf("Plex credentials leaked to media source")
		}
		if request.Header.Get("Range") != "bytes=10-19" {
			t.Errorf("Range header was not forwarded")
		}
		writer.Header().Set("Content-Type", "video/mp4")
		writer.Header().Set("Accept-Ranges", "bytes")
		writer.Header().Set("Content-Range", "bytes 10-19/100")
		writer.WriteHeader(http.StatusPartialContent)
		_, _ = writer.Write([]byte("0123456789"))
	}))
	defer media.Close()

	server := newTestServer(t, media.URL, "proxy")
	defer server.Close()
	server.app.mappings.Put(PartMapping{PartID: "77", Kind: PartKindSTRM, ResolvedURL: media.URL + "/movie.mkv"})

	request, err := http.NewRequest(http.MethodGet, server.URL+"/library/parts/77/1786724686/file", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Range", "bytes=10-19")
	request.Header.Set("X-Plex-Token", "secret")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusPartialContent || string(body) != "0123456789" {
		t.Fatalf("unexpected media response: %d %q", response.StatusCode, body)
	}
	if response.Header.Get("Content-Range") != "bytes 10-19/100" {
		t.Fatalf("Content-Range was not preserved")
	}
	if response.Header.Get("Content-Type") != "video/x-matroska" {
		t.Fatalf("media container type was not corrected: %q", response.Header.Get("Content-Type"))
	}
	if response.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("media CORS header was not set: %q", response.Header.Get("Access-Control-Allow-Origin"))
	}
}

func TestMediaProxySupportsHEAD(t *testing.T) {
	var method string
	media := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		method = request.Method
		writer.Header().Set("Content-Type", "video/mp4")
		writer.Header().Set("Content-Length", "123")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("this body must not reach a HEAD client"))
	}))
	defer media.Close()

	server := newTestServer(t, media.URL, "proxy")
	defer server.Close()
	server.app.mappings.Put(PartMapping{PartID: "88", Kind: PartKindSTRM, ResolvedURL: media.URL + "/movie.mkv"})

	request, err := http.NewRequest(http.MethodHead, server.URL+"/library/parts/88/1786724686/file.mkv", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodHead {
		t.Fatalf("media source received %q instead of HEAD", method)
	}
	if response.StatusCode != http.StatusOK || len(body) != 0 || response.Header.Get("Content-Length") != "123" {
		t.Fatalf("unexpected HEAD response: status=%d body=%d content-length=%q", response.StatusCode, len(body), response.Header.Get("Content-Length"))
	}
}

func TestMediaProxyStreamsBeforeUpstreamCompletes(t *testing.T) {
	firstSent := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseUpstream()

	media := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "video/mp4")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("first-chunk"))
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		close(firstSent)
		<-release
		_, _ = writer.Write([]byte("second-chunk"))
	}))
	defer media.Close()

	server := newTestServer(t, media.URL, "proxy")
	defer server.Close()
	server.app.mappings.Put(PartMapping{PartID: "99", Kind: PartKindSTRM, ResolvedURL: media.URL + "/movie.mkv"})

	type responseResult struct {
		response *http.Response
		err      error
	}
	result := make(chan responseResult, 1)
	go func() {
		response, err := server.Client().Get(server.URL + "/library/parts/99/1786724686/file.mkv")
		result <- responseResult{response: response, err: err}
	}()

	var response *http.Response
	select {
	case value := <-result:
		if value.err != nil {
			t.Fatal(value.err)
		}
		response = value.response
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not return response headers before upstream completed")
	}
	defer response.Body.Close()

	first := make([]byte, len("first-chunk"))
	if _, err := io.ReadFull(response.Body, first); err != nil {
		t.Fatal(err)
	}
	if string(first) != "first-chunk" {
		t.Fatalf("unexpected first streamed chunk: %q", first)
	}
	select {
	case <-firstSent:
	default:
		t.Fatal("upstream did not send the first chunk")
	}
	releaseUpstream()
	rest, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(rest) != "second-chunk" {
		t.Fatalf("unexpected remaining streamed body: %q", rest)
	}
}

func TestParsePartIDSupportsPlexVersionSegment(t *testing.T) {
	for _, path := range []string{
		"/library/parts/77/file",
		"/library/parts/77/1786724686/file",
		"/library/parts/77/1786724686/file.mkv",
	} {
		if partID, ok := parsePartID(path); !ok || partID != "77" {
			t.Fatalf("failed to parse Plex part path %q: %q, %v", path, partID, ok)
		}
	}
}

func TestUnknownPartMappingReturnsExplicitError(t *testing.T) {
	server := newTestServer(t, "http://127.0.0.1:1", "redirect")
	defer server.Close()
	response, err := server.Client().Get(server.URL + "/library/parts/unknown/file")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "mapping") {
		t.Fatalf("unexpected missing mapping response: %d %s", response.StatusCode, body)
	}
}

type testHTTPServer struct {
	*httptest.Server
	app *Server
}

func newTestServer(t *testing.T, plexURL, playbackMode string) *testHTTPServer {
	return newTestServerWithRoot(t, plexURL, t.TempDir(), playbackMode)
}

func newTestServerWithRoot(t *testing.T, plexURL, root, playbackMode string) *testHTTPServer {
	t.Helper()
	upstream, err := url.Parse(plexURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.PlexUpstream = upstream
	cfg.STRMRoots = []string{root}
	cfg.PlaybackMode = playbackMode
	cfg.AllowPrivate = true
	cfg.AllowedPorts = nil
	cfg.DecisionRewrite = true
	cfg.StrictPartMap = true
	app, err := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return &testHTTPServer{Server: httptest.NewServer(app.Handler()), app: app}
}

func TestCachedDirectDecisionReplayRequiresSameSession(t *testing.T) {
	media := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "video/x-matroska")
		writer.Header().Set("Content-Length", "5")
		writer.WriteHeader(http.StatusOK)
		if request.Method != http.MethodHead {
			_, _ = io.WriteString(writer, "media")
		}
	}))
	defer media.Close()

	tools := t.TempDir()
	if err := os.WriteFile(filepath.Join(tools, "ffmpeg"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ffprobe := `#!/bin/sh
cat <<'EOF2'
{"streams":[{"index":0,"codec_type":"video","codec_name":"h264"},{"index":1,"codec_type":"audio","codec_name":"aac"}],"format":{"duration":"1"}}
EOF2
`
	if err := os.WriteFile(filepath.Join(tools, "ffprobe"), []byte(ffprobe), 0o700); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	strmPath := filepath.Join(root, "movie.strm")
	if err := os.WriteFile(strmPath, []byte(media.URL+"/movie.mkv\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var decisionHits int
	var hitsMu sync.Mutex
	plex := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/library/metadata/42":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(writer, `{"MediaContainer":{"Metadata":[{"Media":[{"Part":[{"id":"321","key":"/library/parts/321/file","file":%q}]}]}]}}`, strmPath)
		case "/video/:/transcode/universal/decision":
			hitsMu.Lock()
			decisionHits++
			hitsMu.Unlock()
			writer.Header().Set("Content-Type", "application/xml")
			_, _ = fmt.Fprintf(writer, `<MediaContainer><Video><Media><Part id="321" file=%q decision="directplay" /></Media></Video></MediaContainer>`, strmPath)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer plex.Close()

	upstream, err := url.Parse(plex.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.PlexUpstream = upstream
	cfg.STRMRoots = []string{root}
	cfg.AllowPrivate = true
	cfg.AllowedPorts = nil
	cfg.FFmpegPath = filepath.Join(tools, "ffmpeg")
	app, err := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	app.mappings.Put(PartMapping{
		PartID:      "321",
		Kind:        PartKindSTRM,
		STRMPath:    "/media/movie.strm",
		ResolvedURL: media.URL + "/movie.mkv",
	})
	server := httptest.NewServer(app.Handler())
	defer server.Close()

	decisionPath := url.QueryEscape("/library/metadata/42")
	firstURL := server.URL + "/video/:/transcode/universal/decision?path=" + decisionPath + "&session=session-a&directPlay=1&directStream=0"
	response, err := server.Client().Get(firstURL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `decision="directplay"`) {
		t.Fatalf("unexpected first decision: status=%d body=%s", response.StatusCode, body)
	}
	if decision, ok := app.directDecisionRecord("session-a"); !ok || len(decision.body) == 0 {
		t.Fatal("first direct decision was not remembered for session-a")
	}

	// A different session asking for transcode must not receive session-a's
	// cached direct decision; its request has to reach Plex itself.
	otherURL := server.URL + "/video/:/transcode/universal/decision?path=" + decisionPath + "&session=session-b&directPlay=0&directStream=0"
	response, err = server.Client().Get(otherURL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.Header.Get("X-Plex-Strm-Proxy") == "direct-cache" {
		t.Fatal("cached direct decision was replayed for a different session")
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `decision="directplay"`) {
		t.Fatalf("unexpected other-session decision: status=%d body=%s", response.StatusCode, body)
	}
	hitsMu.Lock()
	hits := decisionHits
	hitsMu.Unlock()
	if hits != 2 {
		t.Fatalf("expected both decisions to reach Plex, upstream hits=%d", hits)
	}

	// The same session retrying with an explicit transcode intent stays direct.
	retryURL := server.URL + "/video/:/transcode/universal/decision?path=" + decisionPath + "&session=session-a&directPlay=0&directStream=0"
	response, err = server.Client().Get(retryURL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("X-Plex-Strm-Proxy") != "direct-cache" {
		t.Fatalf("same-session retry did not use the cached direct decision: status=%d headers=%v", response.StatusCode, response.Header)
	}
	hitsMu.Lock()
	hits = decisionHits
	hitsMu.Unlock()
	if hits != 2 {
		t.Fatalf("same-session replay should not hit Plex again, upstream hits=%d", hits)
	}
}

func TestLocalPartProxyURLFollowsListenAddr(t *testing.T) {
	tests := []struct {
		listenAddr string
		want       string
	}{
		{listenAddr: "0.0.0.0:3001", want: "http://127.0.0.1:3001/library/parts/321/file?plexTranscode=1"},
		{listenAddr: ":3001", want: "http://127.0.0.1:3001/library/parts/321/file?plexTranscode=1"},
		{listenAddr: "127.0.0.1:3999", want: "http://127.0.0.1:3999/library/parts/321/file?plexTranscode=1"},
		{listenAddr: "192.168.1.5:3001", want: "http://192.168.1.5:3001/library/parts/321/file?plexTranscode=1"},
		{listenAddr: "bad", want: "http://127.0.0.1:3001/library/parts/321/file?plexTranscode=1"},
	}
	for _, test := range tests {
		server := &Server{cfg: Config{ListenAddr: test.listenAddr}}
		if got := server.localPartProxyURL("321"); got != test.want {
			t.Fatalf("localPartProxyURL(%q) = %q, want %q", test.listenAddr, got, test.want)
		}
	}
}

func newCacheTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.STRMRoots = []string{t.TempDir()}
	cfg.AllowPrivate = true
	cfg.AllowedPorts = nil
	server, err := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func TestPruneDirectDecisionsBoundsRecords(t *testing.T) {
	server := newCacheTestServer(t)
	now := time.Now()

	server.decisionMu.Lock()
	for i := 0; i < 5; i++ {
		server.directDecisions[fmt.Sprintf("expired-%d", i)] = directDecision{createdAt: now.Add(-time.Hour), expiresAt: now.Add(-time.Minute)}
	}
	for i := 0; i < directDecisionSessionLimit+10; i++ {
		createdAt := now.Add(time.Duration(i) * time.Second)
		server.directDecisions[fmt.Sprintf("live-%d", i)] = directDecision{createdAt: createdAt, expiresAt: now.Add(time.Hour)}
		server.directByPart[fmt.Sprintf("%d", i)] = directDecision{createdAt: createdAt, expiresAt: now.Add(time.Hour)}
	}
	server.pruneDirectDecisionsLocked(now)
	sessions, parts := len(server.directDecisions), len(server.directByPart)
	server.decisionMu.Unlock()

	if sessions > directDecisionSessionLimit {
		t.Fatalf("session cache not bounded: %d entries", sessions)
	}
	if parts > directDecisionSessionLimit {
		t.Fatalf("part cache not bounded: %d entries", parts)
	}
	for i := 0; i < 5; i++ {
		if _, ok := server.directDecisions[fmt.Sprintf("expired-%d", i)]; ok {
			t.Fatalf("expired decision expired-%d survived pruning", i)
		}
	}
	if _, ok := server.directDecisions["live-0"]; ok {
		t.Fatal("oldest live decision was not evicted first")
	}
	if _, ok := server.directDecisions[fmt.Sprintf("live-%d", directDecisionSessionLimit+9)]; !ok {
		t.Fatal("newest live decision should survive pruning")
	}
}

func TestRememberMetadataPartLookupsResetsWhenFull(t *testing.T) {
	server := newCacheTestServer(t)
	server.mappings.Put(PartMapping{
		PartID:      "321",
		Kind:        PartKindSTRM,
		STRMPath:    "/media/movie.strm",
		ResolvedURL: "https://media.example/movie.mkv",
	})

	now := time.Now()
	server.metadataMu.Lock()
	for i := 0; i < metadataPartLookupMaxEntries+5; i++ {
		server.metadataLookups[fmt.Sprintf("/library/metadata/%d\x00%d\x00%d", i, 0, 0)] = metadataPartLookupCacheEntry{
			lookup:    metadataPartLookup{},
			expiresAt: now.Add(30 * time.Second),
		}
	}
	server.metadataMu.Unlock()

	body := []byte(`<MediaContainer><Video><Media><Part id="321" file="/media/movie.strm" /></Media></Video></MediaContainer>`)
	server.rememberMetadataPartLookups("/library/metadata/42", "application/xml", body)

	server.metadataMu.Lock()
	size := len(server.metadataLookups)
	_, hasNew := server.metadataLookups[metadataPartLookupKey("/library/metadata/42", 0, 0)]
	server.metadataMu.Unlock()
	if size > metadataPartLookupMaxEntries {
		t.Fatalf("metadata lookup cache not bounded: %d entries", size)
	}
	if !hasNew {
		t.Fatal("fresh lookup was not cached after reset")
	}
}
