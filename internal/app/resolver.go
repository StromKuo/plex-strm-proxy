package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	ErrSTRMNotFound      = errors.New("strm file not found")
	ErrPathOutsideRoot   = errors.New("path is outside configured STRM roots")
	ErrEmptySTRM         = errors.New("strm file is empty")
	ErrUnsupportedScheme = errors.New("strm URL must use http or https")
	ErrSTRMTooLarge      = errors.New("strm file exceeds configured size limit")
)

type ResolvedSTRM struct {
	Path    string
	URL     string
	ModTime time.Time
	Size    int64
}

type resolverRoot struct {
	Original string
	Absolute string
	Real     string
}

type resolverCacheEntry struct {
	value     ResolvedSTRM
	expiresAt time.Time
}

// Resolver reads STRM files only from configured roots and caches successful
// resolutions. Symlinks are evaluated before the root check so a symlink cannot
// escape the configured media directory.
type Resolver struct {
	roots   []resolverRoot
	maxSize int64
	ttl     time.Duration
	now     func() time.Time
	mu      sync.Mutex
	cache   map[string]resolverCacheEntry
}

func NewResolver(roots []string, maxSize int64, ttl time.Duration) (*Resolver, error) {
	if len(roots) == 0 {
		return nil, fmt.Errorf("at least one STRM root is required")
	}
	if maxSize <= 0 || ttl <= 0 {
		return nil, fmt.Errorf("resolver size limit and TTL must be positive")
	}

	resolvedRoots := make([]resolverRoot, 0, len(roots))
	for _, root := range roots {
		absolute, err := filepath.Abs(root)
		if err != nil {
			return nil, fmt.Errorf("resolve STRM root %q: %w", root, err)
		}
		realRoot, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			return nil, fmt.Errorf("resolve STRM root %q: %w", root, err)
		}
		info, err := os.Stat(realRoot)
		if err != nil {
			return nil, fmt.Errorf("stat STRM root %q: %w", root, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("STRM root %q is not a directory", root)
		}
		resolvedRoots = append(resolvedRoots, resolverRoot{Original: root, Absolute: filepath.Clean(absolute), Real: filepath.Clean(realRoot)})
	}

	return &Resolver{
		roots:   resolvedRoots,
		maxSize: maxSize,
		ttl:     ttl,
		now:     time.Now,
		cache:   make(map[string]resolverCacheEntry),
	}, nil
}

func (r *Resolver) Resolve(ctx context.Context, requestedPath string) (ResolvedSTRM, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ResolvedSTRM{}, ctx.Err()
	default:
	}

	decoded, err := url.PathUnescape(requestedPath)
	if err != nil {
		return ResolvedSTRM{}, fmt.Errorf("decode STRM path: %w", err)
	}
	decoded = strings.TrimSpace(decoded)
	if decoded == "" {
		return ResolvedSTRM{}, ErrSTRMNotFound
	}
	if !strings.EqualFold(filepath.Ext(filepath.FromSlash(decoded)), ".strm") {
		return ResolvedSTRM{}, fmt.Errorf("%w: %q is not a .strm path", ErrSTRMNotFound, requestedPath)
	}

	var lastErr error
	for _, root := range r.roots {
		candidate := decoded
		if !filepath.IsAbs(filepath.FromSlash(candidate)) {
			candidate = filepath.Join(root.Absolute, filepath.FromSlash(candidate))
		}
		resolvedPath, stat, err := r.secureFile(root, candidate)
		if err != nil {
			lastErr = err
			continue
		}
		cacheKey := fmt.Sprintf("%s|%d|%d", resolvedPath, stat.ModTime().UnixNano(), stat.Size())
		if value, ok := r.cached(cacheKey); ok {
			return value, nil
		}

		value, err := r.readAndResolve(resolvedPath, stat)
		if err != nil {
			return ResolvedSTRM{}, err
		}
		r.store(cacheKey, value)
		return value, nil
	}
	if lastErr != nil {
		return ResolvedSTRM{}, lastErr
	}
	return ResolvedSTRM{}, ErrSTRMNotFound
}

func (r *Resolver) secureFile(root resolverRoot, candidate string) (string, os.FileInfo, error) {
	candidateAbs, err := filepath.Abs(candidate)
	if err != nil {
		return "", nil, err
	}
	if !withinPath(root.Absolute, candidateAbs) {
		return "", nil, ErrPathOutsideRoot
	}
	realPath, err := filepath.EvalSymlinks(candidateAbs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil, ErrSTRMNotFound
		}
		return "", nil, fmt.Errorf("resolve STRM file: %w", err)
	}
	if !withinPath(root.Real, realPath) {
		return "", nil, ErrPathOutsideRoot
	}
	info, err := os.Stat(realPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil, ErrSTRMNotFound
		}
		return "", nil, fmt.Errorf("stat STRM file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("STRM path is not a regular file")
	}
	return filepath.Clean(realPath), info, nil
}

func (r *Resolver) readAndResolve(path string, info os.FileInfo) (ResolvedSTRM, error) {
	if info.Size() > r.maxSize {
		return ResolvedSTRM{}, ErrSTRMTooLarge
	}
	file, err := os.Open(path)
	if err != nil {
		return ResolvedSTRM{}, fmt.Errorf("open STRM file: %w", err)
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, r.maxSize+1))
	if err != nil {
		return ResolvedSTRM{}, fmt.Errorf("read STRM file: %w", err)
	}
	if int64(len(data)) > r.maxSize {
		return ResolvedSTRM{}, ErrSTRMTooLarge
	}

	scanner := bufio.NewScanner(strings.NewReader(strings.TrimPrefix(string(data), "\ufeff")))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		mediaURL, err := parseMediaURL(line)
		if err != nil {
			return ResolvedSTRM{}, err
		}
		return ResolvedSTRM{Path: path, URL: mediaURL, ModTime: info.ModTime(), Size: info.Size()}, nil
	}
	if err := scanner.Err(); err != nil {
		return ResolvedSTRM{}, fmt.Errorf("parse STRM file: %w", err)
	}
	return ResolvedSTRM{}, ErrEmptySTRM
}

func parseMediaURL(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("invalid STRM URL: %w", err)
	}
	if parsed.Scheme == "" {
		return "", fmt.Errorf("invalid STRM URL: missing scheme")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		parsed.Scheme = strings.ToLower(parsed.Scheme)
	default:
		return "", fmt.Errorf("%w: %q", ErrUnsupportedScheme, parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("invalid STRM URL: missing host")
	}
	if parsed.User != nil {
		return "", fmt.Errorf("invalid STRM URL: userinfo is not allowed")
	}
	return parsed.String(), nil
}

func withinPath(root, candidate string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ""
}

func (r *Resolver) cached(key string) (ResolvedSTRM, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.cache[key]
	if !ok {
		return ResolvedSTRM{}, false
	}
	if !r.now().Before(entry.expiresAt) {
		delete(r.cache, key)
		return ResolvedSTRM{}, false
	}
	return entry.value, true
}

func (r *Resolver) store(key string, value ResolvedSTRM) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache[key] = resolverCacheEntry{value: value, expiresAt: r.now().Add(r.ttl)}
}
