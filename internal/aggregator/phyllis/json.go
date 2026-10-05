package phyllis

import "encoding/json"

// stringField extracts a single string field from a JSON object body. Returns
// "" if the body is not a JSON object or the field is missing/non-string.
func stringField(body []byte, field string) string {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	v, _ := m[field].(string)
	return v
}

// mergeJSON merges src JSON object body with the given overlay map and
// returns the marshalled result. If src is not a JSON object, the overlay is
// returned standalone.
func mergeJSON(src []byte, overlay map[string]any) []byte {
	var m map[string]any
	if err := json.Unmarshal(src, &m); err != nil || m == nil {
		m = map[string]any{}
	}
	for k, v := range overlay {
		m[k] = v
	}
	return marshalJSON(m)
}

// rawJSONOrNull returns the raw JSON message wrapped as a json.RawMessage so
// it can be embedded inside a parent map without losing structure. If the
// input is not valid JSON, returns nil.
func rawJSONOrNull(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil
	}
	return v
}

// marshalJSON marshals v, returning a {} placeholder on encoding error so
// downstream consumers always receive a valid JSON object.
func marshalJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}
