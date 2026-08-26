package app

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"mime"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type PartKind string

const (
	PartKindLocal PartKind = "local"
	PartKindSTRM  PartKind = "strm"
)

type PartMapping struct {
	PartID        string
	Kind          PartKind
	File          string
	Key           string
	STRMPath      string
	ResolvedURL   string
	UpdatedAt     time.Time
	ResolutionErr string
}

type mappingEntry struct {
	value     PartMapping
	expiresAt time.Time
}

type MappingStore struct {
	mu    sync.RWMutex
	ttl   time.Duration
	now   func() time.Time
	items map[string]mappingEntry
}

func NewMappingStore(ttl time.Duration) (*MappingStore, error) {
	if ttl <= 0 {
		return nil, fmt.Errorf("mapping TTL must be positive")
	}
	return &MappingStore{ttl: ttl, now: time.Now, items: make(map[string]mappingEntry)}, nil
}

func (s *MappingStore) Get(partID string) (PartMapping, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.items[partID]
	if !ok {
		return PartMapping{}, false
	}
	if !s.now().Before(entry.expiresAt) {
		delete(s.items, partID)
		return PartMapping{}, false
	}
	return entry.value, true
}

func (s *MappingStore) Put(mapping PartMapping) {
	if mapping.PartID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if existing, ok := s.items[mapping.PartID]; ok && now.Before(existing.expiresAt) && existing.value.Kind == PartKindSTRM && existing.value.ResolvedURL != "" && mapping.Kind == PartKindLocal {
		// Plex may later echo the same Part through a session/status response
		// without the original .strm file. That representation is not a new
		// library fact and must not downgrade the authoritative STRM mapping;
		// otherwise the next Part request is sent back to Plex and can produce
		// an upstream redirect/auth response instead of the configured 302.
		mapping = existing.value
	}
	if mapping.UpdatedAt.IsZero() {
		mapping.UpdatedAt = now
	}
	s.items[mapping.PartID] = mappingEntry{value: mapping, expiresAt: now.Add(s.ttl)}
}

func (s *MappingStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.items)
}

type partRecord struct {
	ID       string
	File     string
	Key      string
	Path     string
	Selected bool
}

func (s *MappingStore) IngestStructuredResponse(contentType string, body []byte, defaultSTRMPath string, resolver *Resolver) []PartMapping {
	records := extractPartRecords(contentType, body)
	result := make([]PartMapping, 0, len(records))
	for index, record := range records {
		partIDs := mappingPartIDs(record)
		if len(partIDs) == 0 {
			continue
		}
		strmPath := ""
		for _, candidate := range []string{record.File, record.Path, record.Key} {
			if isSTRMPath(candidate) {
				strmPath = candidate
				break
			}
		}
		if strmPath == "" && len(records) == 1 && index == 0 && isSTRMPath(defaultSTRMPath) {
			strmPath = defaultSTRMPath
		}

		for _, partID := range partIDs {
			mapping := PartMapping{
				PartID:    partID,
				Kind:      PartKindLocal,
				File:      record.File,
				Key:       record.Key,
				STRMPath:  strmPath,
				UpdatedAt: s.now(),
			}
			if strmPath != "" {
				mapping.Kind = PartKindSTRM
				if resolver != nil {
					resolved, err := resolver.Resolve(nil, strmPath)
					if err != nil {
						mapping.ResolutionErr = err.Error()
					} else {
						mapping.ResolvedURL = resolved.URL
					}
				}
			}
			s.Put(mapping)
			result = append(result, mapping)
		}
	}
	return result
}

func mappingPartIDs(record partRecord) []string {
	ids := make([]string, 0, 2)
	appendID := func(value string) {
		if !safePartID(value) {
			return
		}
		for _, existing := range ids {
			if existing == value {
				return
			}
		}
		ids = append(ids, value)
	}
	appendID(record.ID)
	if keyID, ok := parsePartID(record.Key); ok {
		appendID(keyID)
	}
	return ids
}

func extractPartRecords(contentType string, body []byte) []partRecord {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = strings.TrimSpace(strings.Split(contentType, ";")[0])
	}
	switch strings.ToLower(mediaType) {
	case "application/xml", "text/xml", "application/plex+xml":
		return extractXMLPartRecords(body)
	case "application/json", "text/json":
		return extractJSONPartRecords(body)
	default:
		return nil
	}
}

func extractXMLPartRecords(body []byte) []partRecord {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	var records []partRecord
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "Part" {
			continue
		}
		records = append(records, partRecordFromXMLAttrs(start.Attr))
	}
	return records
}

func extractJSONPartRecords(body []byte) []partRecord {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return nil
	}
	var records []partRecord
	var walk func(any, bool)
	walk = func(node any, inPart bool) {
		switch typed := node.(type) {
		case []any:
			for _, child := range typed {
				walk(child, inPart)
			}
		case map[string]any:
			if inPart {
				record := partRecordFromJSONObject(typed)
				if record.ID != "" {
					records = append(records, record)
				}
			}
			for key, child := range typed {
				walk(child, strings.EqualFold(key, "part"))
			}
		}
	}
	walk(value, false)
	return records
}

func jsonString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case uint:
		return strconv.FormatUint(uint64(typed), 10)
	case uint64:
		return strconv.FormatUint(typed, 10)
	default:
		return ""
	}
}

func jsonBool(value any) bool {
	if parsed, ok := value.(bool); ok {
		return parsed
	}
	return metadataBool(jsonString(value))
}

func isSTRMPath(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if question := strings.IndexByte(value, '?'); question >= 0 {
		value = value[:question]
	}
	return strings.EqualFold(filepath.Ext(filepath.FromSlash(value)), ".strm")
}

func safePartID(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '-' && char != '_' {
			return false
		}
	}
	return true
}
