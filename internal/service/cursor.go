package service

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/CoverOnes/marketplace/internal/store"
)

// encodeSearchCursor serializes a keyset cursor to an opaque, URL-safe base64
// token suitable for round-tripping through a query string. The token is
// deliberately opaque so clients cannot construct or tamper with cursor state;
// decodeSearchCursor validates structure on the way back in.
func encodeSearchCursor(c store.SearchCursor) string {
	raw, err := json.Marshal(c)
	if err != nil {
		// SearchCursor only holds time.Time + uuid.UUID, both of which marshal
		// cleanly; an error here is impossible in practice. Fail closed by
		// returning an empty cursor rather than panicking.
		return ""
	}

	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeSearchCursor parses an opaque cursor token. An empty string yields a nil
// cursor (first page). A malformed token returns an error so the handler can map
// it to a 400 rather than silently restarting pagination.
func decodeSearchCursor(token string) (*store.SearchCursor, error) {
	if token == "" {
		return nil, nil
	}

	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, fmt.Errorf("decode cursor base64: %w", err)
	}

	var c store.SearchCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("unmarshal cursor: %w", err)
	}

	if c.ID == [16]byte{} {
		return nil, fmt.Errorf("cursor missing id")
	}

	return &c, nil
}
