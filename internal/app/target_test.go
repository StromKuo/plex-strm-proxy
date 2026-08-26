package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestResolveMediaTargetValidatesRedirectsWithHEAD(t *testing.T) {
	tests := []struct {
		name       string
		headStatus int
		wantMethod []string
	}{
		{name: "head supported", headStatus: http.StatusOK, wantMethod: []string{http.MethodHead, http.MethodHead}},
		{name: "get fallback on method not allowed", headStatus: http.StatusMethodNotAllowed, wantMethod: []string{http.MethodHead, http.MethodGet, http.MethodGet}},
		{name: "get fallback on not implemented", headStatus: http.StatusNotImplemented, wantMethod: []string{http.MethodHead, http.MethodGet, http.MethodGet}},
		{name: "get fallback on bad request", headStatus: http.StatusBadRequest, wantMethod: []string{http.MethodHead, http.MethodGet, http.MethodGet}},
		{name: "get fallback on forbidden", headStatus: http.StatusForbidden, wantMethod: []string{http.MethodHead, http.MethodGet, http.MethodGet}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var mu sync.Mutex
			var methods []string
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				mu.Lock()
				methods = append(methods, request.Method)
				mu.Unlock()
				switch request.URL.Path {
				case "/source":
					if request.Method == http.MethodHead && test.headStatus != http.StatusOK {
						writer.WriteHeader(test.headStatus)
						return
					}
					writer.Header().Set("Location", server.URL+"/media")
					writer.WriteHeader(http.StatusFound)
				case "/media":
					writer.Header().Set("Content-Length", "1")
					writer.WriteHeader(http.StatusOK)
				default:
					http.NotFound(writer, request)
				}
			}))
			defer server.Close()

			policy := TargetPolicy{
				AllowHTTP:     true,
				AllowPrivate:  true,
				MaxRedirects:  3,
				HeaderTimeout: time.Second,
			}
			client := NewMediaClient(policy)
			got, err := ResolveMediaTarget(context.Background(), client, policy, server.URL+"/source")
			if err != nil {
				t.Fatalf("ResolveMediaTarget returned error: %v", err)
			}
			if got != server.URL+"/media" {
				t.Fatalf("resolved URL = %q, want %q", got, server.URL+"/media")
			}
			mu.Lock()
			gotMethods := append([]string(nil), methods...)
			mu.Unlock()
			if !reflect.DeepEqual(gotMethods, test.wantMethod) {
				t.Fatalf("request methods = %v, want %v", gotMethods, test.wantMethod)
			}
		})
	}
}

func TestResolveMediaTargetRetriesGETWhenHEADFails(t *testing.T) {
	var methods []string
	client := &http.Client{
		Transport: targetRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			methods = append(methods, request.Method)
			if request.Method == http.MethodHead {
				return nil, context.DeadlineExceeded
			}
			finalURL, err := url.Parse("https://media.example/video.mp4")
			if err != nil {
				return nil, err
			}
			finalRequest := request.Clone(request.Context())
			finalRequest.URL = finalURL
			return &http.Response{
				StatusCode: http.StatusPartialContent,
				Request:    finalRequest,
				Body:       io.NopCloser(strings.NewReader("x")),
			}, nil
		}),
	}
	policy := TargetPolicy{
		AllowHTTPS:   true,
		AllowPrivate: true,
		AllowedPorts: []int{443},
	}

	got, err := ResolveMediaTarget(context.Background(), client, policy, "https://media.example/source")
	if err != nil {
		t.Fatalf("ResolveMediaTarget returned error: %v", err)
	}
	if got != "https://media.example/video.mp4" {
		t.Fatalf("resolved URL = %q, want https://media.example/video.mp4", got)
	}
	if !reflect.DeepEqual(methods, []string{http.MethodHead, http.MethodGet}) {
		t.Fatalf("request methods = %v, want [HEAD GET]", methods)
	}
}

func TestResolveMediaTargetRejectsLoginPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = writer.Write([]byte("<html>login</html>"))
	}))
	defer server.Close()

	policy := TargetPolicy{
		AllowHTTP:    true,
		AllowPrivate: true,
		MaxRedirects: 3,
	}
	_, err := ResolveMediaTarget(context.Background(), NewMediaClient(policy), policy, server.URL+"/login")
	if err == nil || !strings.Contains(err.Error(), "login page") {
		t.Fatalf("ResolveMediaTarget error = %v, want login page rejection", err)
	}
}

func TestResolveMediaTargetRetriesGETWhenHEADResolvesLoginPage(t *testing.T) {
	var methods []string
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		methods = append(methods, request.Method+" "+request.URL.Path)
		switch request.URL.Path {
		case "/source":
			writer.Header().Set("Location", server.URL+map[bool]string{true: "/login", false: "/media"}[request.Method == http.MethodHead])
			writer.WriteHeader(http.StatusFound)
		case "/login":
			writer.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = writer.Write([]byte("<html>login</html>"))
		case "/media":
			writer.Header().Set("Content-Type", "video/mp4")
			writer.WriteHeader(http.StatusPartialContent)
			_, _ = writer.Write([]byte("x"))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	policy := TargetPolicy{
		AllowHTTP:    true,
		AllowPrivate: true,
		MaxRedirects: 3,
	}
	got, err := ResolveMediaTarget(context.Background(), NewMediaClient(policy), policy, server.URL+"/source")
	if err != nil {
		t.Fatalf("ResolveMediaTarget returned error: %v", err)
	}
	if got != server.URL+"/media" {
		t.Fatalf("resolved URL = %q, want %q", got, server.URL+"/media")
	}
	wantMethods := []string{http.MethodHead + " /source", http.MethodHead + " /login", http.MethodGet + " /source", http.MethodGet + " /media"}
	if !reflect.DeepEqual(methods, wantMethods) {
		t.Fatalf("request methods = %v, want %v", methods, wantMethods)
	}
}

type targetRoundTripFunc func(*http.Request) (*http.Response, error)

func (f targetRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
