package app

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type metadataPartLookup struct {
	Mapping     PartMapping
	ContentType string
	Body        []byte
}

type metadataPartLookupCacheEntry struct {
	lookup    metadataPartLookup
	expiresAt time.Time
}

const metadataPartLookupTTL = 30 * time.Second

// metadataPartLookupMaxEntries bounds the metadata part lookup cache. Entries
// hold full metadata bodies, so the cache is reset once it grows past this
// many distinct (path, media, part) keys.
const metadataPartLookupMaxEntries = 256

func (s *Server) lookupMetadataPart(ctx context.Context, source *http.Request, metadataPath string) (PartMapping, error) {
	lookup, err := s.lookupMetadataPartSnapshot(ctx, source, metadataPath)
	if err != nil {
		return PartMapping{}, err
	}
	return lookup.Mapping, nil
}

func (s *Server) lookupMetadataPartSnapshot(ctx context.Context, source *http.Request, metadataPath string) (metadataPartLookup, error) {
	mediaIndex, err := queryIndex(source.URL.Query(), "mediaIndex")
	if err != nil {
		return metadataPartLookup{}, err
	}
	partIndex, err := queryIndex(source.URL.Query(), "partIndex")
	if err != nil {
		return metadataPartLookup{}, err
	}
	if lookup, ok := s.cachedMetadataPartLookup(metadataPath, mediaIndex, partIndex); ok {
		return lookup, nil
	}

	metadataURL := *s.cfg.PlexUpstream
	metadataURL.Path = joinPlexPath(s.cfg.PlexUpstream.Path, metadataPath)
	metadataURL.RawPath = ""
	metadataQuery := url.Values{
		"checkFiles":           {"1"},
		"includeBandwidths":    {"1"},
		"includeExternalMedia": {"1"},
	}
	if token := source.URL.Query().Get("X-Plex-Token"); token != "" {
		metadataQuery.Set("X-Plex-Token", token)
	}
	metadataURL.RawQuery = metadataQuery.Encode()

	metadataRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataURL.String(), nil)
	if err != nil {
		return metadataPartLookup{}, fmt.Errorf("create Plex metadata request: %w", err)
	}
	copyPlexRequestHeaders(metadataRequest.Header, source.Header)
	accept := strings.TrimSpace(source.Header.Get("Accept"))
	if accept == "" {
		accept = "application/xml"
	}
	metadataRequest.Header.Set("Accept", accept)

	response, err := s.plexClient.Do(metadataRequest)
	if err != nil {
		return metadataPartLookup{}, fmt.Errorf("request Plex metadata: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return metadataPartLookup{}, fmt.Errorf("Plex metadata returned HTTP %d", response.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, s.cfg.MaxPlexBodyBytes+1))
	if err != nil {
		return metadataPartLookup{}, fmt.Errorf("read Plex metadata: %w", err)
	}
	if int64(len(body)) > s.cfg.MaxPlexBodyBytes {
		return metadataPartLookup{}, fmt.Errorf("Plex metadata exceeds configured size limit")
	}
	decoded, err := decodeStructuredBody(response.Header.Get("Content-Encoding"), body, s.cfg.MaxPlexBodyBytes)
	if err != nil {
		return metadataPartLookup{}, fmt.Errorf("decode Plex metadata: %w", err)
	}

	record, err := selectMetadataPart(response.Header.Get("Content-Type"), decoded, mediaIndex, partIndex)
	if err != nil {
		return metadataPartLookup{}, err
	}
	s.mappings.IngestStructuredResponse(response.Header.Get("Content-Type"), decoded, "", s.resolver)
	for _, partID := range mappingPartIDs(record) {
		if mapping, ok := s.mappings.Get(partID); ok {
			lookup := metadataPartLookup{Mapping: mapping, ContentType: response.Header.Get("Content-Type"), Body: decoded}
			s.rememberMetadataPartLookups(metadataPath, lookup.ContentType, decoded)
			return lookup, nil
		}
	}
	return metadataPartLookup{}, fmt.Errorf("metadata Part has no usable id: mediaIndex=%d partIndex=%d", mediaIndex, partIndex)
}

func metadataPartLookupKey(metadataPath string, mediaIndex, partIndex int) string {
	return fmt.Sprintf("%s\x00%d\x00%d", metadataPath, mediaIndex, partIndex)
}

func (s *Server) cachedMetadataPartLookup(metadataPath string, mediaIndex, partIndex int) (metadataPartLookup, bool) {
	key := metadataPartLookupKey(metadataPath, mediaIndex, partIndex)
	s.metadataMu.Lock()
	defer s.metadataMu.Unlock()
	entry, ok := s.metadataLookups[key]
	if !ok {
		return metadataPartLookup{}, false
	}
	if !time.Now().Before(entry.expiresAt) {
		delete(s.metadataLookups, key)
		return metadataPartLookup{}, false
	}
	return entry.lookup, true
}

func (s *Server) rememberMetadataPartLookups(metadataPath, contentType string, body []byte) {
	if metadataPath == "" || len(body) == 0 {
		return
	}
	indexed := extractIndexedPartRecords(contentType, body)
	if len(indexed) == 0 {
		return
	}
	ttl := metadataPartLookupTTL
	if s.cfg.MappingCacheTTL < ttl {
		ttl = s.cfg.MappingCacheTTL
	}
	expiresAt := time.Now().Add(ttl)
	s.metadataMu.Lock()
	defer s.metadataMu.Unlock()
	s.pruneMetadataLookupsLocked(time.Now())
	if len(s.metadataLookups) >= metadataPartLookupMaxEntries {
		// Short-TTL lookup cache: a wholesale reset bounds memory without
		// LRU bookkeeping when many distinct metadata paths are seen at once.
		s.metadataLookups = make(map[string]metadataPartLookupCacheEntry)
	}
	for mediaIndex, parts := range indexed {
		for partIndex, record := range parts {
			var mapping PartMapping
			found := false
			for _, partID := range mappingPartIDs(record) {
				if value, ok := s.mappings.Get(partID); ok {
					mapping = value
					found = true
					break
				}
			}
			if !found {
				continue
			}
			key := metadataPartLookupKey(metadataPath, mediaIndex, partIndex)
			s.metadataLookups[key] = metadataPartLookupCacheEntry{
				lookup: metadataPartLookup{
					Mapping:     mapping,
					ContentType: contentType,
					Body:        body,
				},
				expiresAt: expiresAt,
			}
		}
	}
}

// pruneMetadataLookupsLocked drops expired metadata part lookups.
func (s *Server) pruneMetadataLookupsLocked(now time.Time) {
	for key, entry := range s.metadataLookups {
		if !now.Before(entry.expiresAt) {
			delete(s.metadataLookups, key)
		}
	}
}

func queryIndex(query url.Values, name string) (int, error) {
	value := strings.TrimSpace(query.Get(name))
	if value == "" {
		return 0, nil
	}
	index, err := strconv.Atoi(value)
	if err != nil || index < 0 {
		return 0, fmt.Errorf("invalid %s %q", name, value)
	}
	return index, nil
}

func joinPlexPath(basePath, requestPath string) string {
	basePath = strings.TrimRight(basePath, "/")
	if basePath == "" {
		return requestPath
	}
	return basePath + "/" + strings.TrimLeft(requestPath, "/")
}

func copyPlexRequestHeaders(destination, source http.Header) {
	for _, name := range []string{
		"Accept-Language",
		"Authorization",
		"User-Agent",
		"X-Plex-Client-Identifier",
		"X-Plex-Device",
		"X-Plex-Device-Name",
		"X-Plex-Platform",
		"X-Plex-Platform-Version",
		"X-Plex-Product",
		"X-Plex-Protocol",
		"X-Plex-Protocol-Version",
		"X-Plex-Session-Identifier",
		"X-Plex-Target-Client-Identifier",
		"X-Plex-Token",
	} {
		for _, value := range source.Values(name) {
			destination.Add(name, value)
		}
	}
}

func selectMetadataPart(contentType string, body []byte, mediaIndex, partIndex int) (partRecord, error) {
	mediaParts := extractIndexedPartRecords(contentType, body)
	if mediaIndex >= len(mediaParts) || partIndex >= len(mediaParts[mediaIndex]) {
		return partRecord{}, fmt.Errorf("Plex metadata Part not found: mediaIndex=%d partIndex=%d", mediaIndex, partIndex)
	}
	return mediaParts[mediaIndex][partIndex], nil
}

func extractIndexedPartRecords(contentType string, body []byte) [][]partRecord {
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	switch mediaType {
	case "application/xml", "text/xml", "application/plex+xml":
		return extractXMLMediaPartRecords(body)
	case "application/json", "text/json":
		return extractJSONMediaPartRecords(body)
	default:
		return nil
	}
}

func extractXMLMediaPartRecords(body []byte) [][]partRecord {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	var result [][]partRecord
	currentMedia := -1
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		switch typed := token.(type) {
		case xml.StartElement:
			if typed.Name.Local == "Media" {
				result = append(result, nil)
				currentMedia = len(result) - 1
				continue
			}
			if typed.Name.Local != "Part" || currentMedia < 0 {
				continue
			}
			result[currentMedia] = append(result[currentMedia], partRecordFromXMLAttrs(typed.Attr))
		case xml.EndElement:
			if typed.Name.Local == "Media" {
				currentMedia = -1
			}
		}
	}
	return result
}

func extractJSONMediaPartRecords(body []byte) [][]partRecord {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return nil
	}
	var result [][]partRecord
	var walk func(any)
	walk = func(node any) {
		object, ok := node.(map[string]any)
		if !ok {
			if list, ok := node.([]any); ok {
				for _, child := range list {
					walk(child)
				}
			}
			return
		}
		for key, child := range object {
			if !strings.EqualFold(key, "Media") {
				walk(child)
				continue
			}
			mediaList, ok := child.([]any)
			if !ok {
				mediaList = []any{child}
			}
			for _, mediaNode := range mediaList {
				mediaObject, ok := mediaNode.(map[string]any)
				if !ok {
					continue
				}
				partValue, ok := objectValue(mediaObject, "Part")
				if !ok {
					continue
				}
				partList, ok := partValue.([]any)
				if !ok {
					partList = []any{partValue}
				}
				var records []partRecord
				for _, partNode := range partList {
					if partObject, ok := partNode.(map[string]any); ok {
						records = append(records, partRecordFromJSONObject(partObject))
					}
				}
				if len(records) > 0 {
					result = append(result, records)
				}
			}
		}
	}
	walk(value)
	return result
}

// selectedPlayQueuePartIDs returns the Part IDs for the item Plex selected in
// a play queue. Plex normally records this selection on the queue container
// and the Video item, rather than on Part.selected. The latter is retained as
// a fallback for older responses and synthetic test fixtures.
func selectedPlayQueuePartIDs(contentType string, body []byte) map[string]bool {
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	switch mediaType {
	case "application/xml", "text/xml", "application/plex+xml":
		return selectedXMLPlayQueuePartIDs(body)
	case "application/json", "text/json":
		return selectedJSONPlayQueuePartIDs(body)
	default:
		return make(map[string]bool)
	}
}

func selectedXMLPlayQueuePartIDs(body []byte) map[string]bool {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	selected := make(map[string]bool)
	var selectedItemID, selectedMetadataID string
	var allParts []partRecord

	type queueVideo struct {
		ratingKey       string
		playQueueItemID string
		parts           []partRecord
	}
	var current *queueVideo
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		switch typed := token.(type) {
		case xml.StartElement:
			switch typed.Name.Local {
			case "MediaContainer":
				selectedItemID = xmlAttributeValue(typed.Attr, "playQueueSelectedItemID")
				selectedMetadataID = xmlAttributeValue(typed.Attr, "playQueueSelectedMetadataItemID")
			case "Video":
				current = &queueVideo{
					ratingKey:       xmlAttributeValue(typed.Attr, "ratingKey"),
					playQueueItemID: xmlAttributeValue(typed.Attr, "playQueueItemID"),
				}
			case "Part":
				record := partRecordFromXMLAttrs(typed.Attr)
				allParts = append(allParts, record)
				if current != nil {
					current.parts = append(current.parts, record)
				}
			}
		case xml.EndElement:
			if typed.Name.Local != "Video" || current == nil {
				continue
			}
			matches := (selectedMetadataID != "" && current.ratingKey == selectedMetadataID) ||
				(selectedItemID != "" && current.playQueueItemID == selectedItemID)
			if matches {
				for _, record := range current.parts {
					addPartRecordIDs(selected, record)
				}
			}
			current = nil
		}
	}
	if len(selected) == 0 {
		for _, record := range allParts {
			if record.Selected {
				addPartRecordIDs(selected, record)
			}
		}
	}
	return selected
}

func selectedJSONPlayQueuePartIDs(body []byte) map[string]bool {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return make(map[string]bool)
	}
	root, ok := value.(map[string]any)
	if !ok {
		return make(map[string]bool)
	}
	if container, found := objectValue(root, "MediaContainer"); found {
		if containerObject, isObject := container.(map[string]any); isObject {
			root = containerObject
		}
	}
	selectedItemID := jsonObjectString(root, "playQueueSelectedItemID")
	selectedMetadataID := jsonObjectString(root, "playQueueSelectedMetadataItemID")
	selected := make(map[string]bool)
	var walk func(any)
	walk = func(node any) {
		switch typed := node.(type) {
		case []any:
			for _, child := range typed {
				walk(child)
			}
		case map[string]any:
			ratingKey := jsonObjectString(typed, "ratingKey")
			playQueueItemID := jsonObjectString(typed, "playQueueItemID")
			matches := (selectedMetadataID != "" && ratingKey == selectedMetadataID) ||
				(selectedItemID != "" && playQueueItemID == selectedItemID)
			if matches {
				addJSONMediaPartIDs(typed, selected)
			}
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(root)
	if len(selected) == 0 {
		for _, record := range extractPartRecords("application/json", body) {
			if record.Selected {
				addPartRecordIDs(selected, record)
			}
		}
	}
	return selected
}

func addJSONMediaPartIDs(object map[string]any, selected map[string]bool) {
	mediaValue, ok := objectValue(object, "Media")
	if !ok {
		return
	}
	mediaList, ok := mediaValue.([]any)
	if !ok {
		mediaList = []any{mediaValue}
	}
	for _, mediaNode := range mediaList {
		mediaObject, ok := mediaNode.(map[string]any)
		if !ok {
			continue
		}
		partValue, ok := objectValue(mediaObject, "Part")
		if !ok {
			continue
		}
		partList, ok := partValue.([]any)
		if !ok {
			partList = []any{partValue}
		}
		for _, partNode := range partList {
			partObject, ok := partNode.(map[string]any)
			if ok {
				addPartRecordIDs(selected, partRecordFromJSONObject(partObject))
			}
		}
	}
}

func addPartRecordIDs(selected map[string]bool, record partRecord) {
	for _, partID := range mappingPartIDs(record) {
		selected[partID] = true
	}
}

func objectValue(object map[string]any, name string) (any, bool) {
	for key, value := range object {
		if strings.EqualFold(key, name) {
			return value, true
		}
	}
	return nil, false
}

func partRecordFromXMLAttrs(attributes []xml.Attr) partRecord {
	var record partRecord
	for _, attribute := range attributes {
		switch strings.ToLower(attribute.Name.Local) {
		case "id":
			record.ID = attribute.Value
		case "file":
			record.File = attribute.Value
		case "key":
			record.Key = attribute.Value
		case "path":
			record.Path = attribute.Value
		case "selected":
			record.Selected = metadataBool(attribute.Value)
		}
	}
	return record
}

func partRecordFromJSONObject(object map[string]any) partRecord {
	return partRecord{
		ID:       jsonObjectString(object, "id"),
		File:     jsonObjectString(object, "file"),
		Key:      jsonObjectString(object, "key"),
		Path:     jsonObjectString(object, "path"),
		Selected: jsonBool(jsonObjectValue(object, "selected")),
	}
}

func metadataBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

func jsonObjectString(object map[string]any, name string) string {
	value, ok := objectValue(object, name)
	if !ok {
		return ""
	}
	return jsonString(value)
}
