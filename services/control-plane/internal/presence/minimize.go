package presence

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

var coarseFieldsForCategory = map[Category]map[string]struct{}{
	CategoryServerVoice: {"channel_id": {}, "server_id": {}, "started_at": {}},
	CategoryPrivateCall: {"call_type": {}, "started_at": {}},
}

// ApplyMinimization returns an exact JSON object containing only coarse fields.
func ApplyMinimization(category Category, rawJSON []byte) ([]byte, error) {
	allow, ok := coarseFieldsForCategory[category]
	if !ok {
		return nil, fmt.Errorf("unsupported presence category")
	}

	decoder := json.NewDecoder(bytes.NewReader(rawJSON))
	var full map[string]json.RawMessage
	if err := decoder.Decode(&full); err != nil || full == nil {
		return nil, fmt.Errorf("decode presence payload")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, fmt.Errorf("decode presence payload trailing data")
	}

	minimized := make(map[string]json.RawMessage, len(allow))
	for field := range allow {
		if value, exists := full[field]; exists {
			minimized[field] = value
		}
	}
	encoded, err := json.Marshal(minimized)
	if err != nil {
		return nil, fmt.Errorf("encode minimized presence payload")
	}
	return encoded, nil
}
