package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
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
