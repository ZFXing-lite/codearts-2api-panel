package api

import (
	"encoding/json"
	"io"
)

// maxJSONDepth limits nesting depth to prevent stack exhaustion from
// malicious deeply-nested JSON (BUG-17 fix).
const maxJSONDepth = 100

// limitedDecoder wraps json.Decoder to reject deeply-nested JSON.
type limitedDecoder struct {
	dec *json.Decoder
}

// maxBody <= 0 时用 16MiB 兜底（历史行为）。
func newLimitedDecoder(r io.Reader, maxBody ...int64) *limitedDecoder {
	size := int64(16 << 20)
	if len(maxBody) > 0 && maxBody[0] > 0 {
		size = maxBody[0]
	}
	dec := json.NewDecoder(io.LimitReader(r, size))
	return &limitedDecoder{dec: dec}
}

// Decode unmarshals into v, rejecting JSON with nesting depth > maxJSONDepth.
func (ld *limitedDecoder) Decode(v any) error {
	// Use json.RawMessage first to check depth, then unmarshal.
	var raw json.RawMessage
	if err := ld.dec.Decode(&raw); err != nil {
		return err
	}
	if depth := jsonDepth(raw); depth > maxJSONDepth {
		return errJSONTooDeep
	}
	return json.Unmarshal(raw, v)
}

// errJSONTooDeep is returned when JSON nesting exceeds maxJSONDepth.
var errJSONTooDeep = &jsonDepthError{msg: "json nesting depth exceeds limit"}

type jsonDepthError struct{ msg string }

func (e *jsonDepthError) Error() string { return e.msg }

// jsonDepth calculates the maximum nesting depth of a JSON byte slice.
// It tracks {/[ open/close to determine depth without full parse.
func jsonDepth(data []byte) int {
	maxDepth := 0
	depth := 0
	inString := false
	escape := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if escape {
			escape = false
			continue
		}
		if c == '\\' && inString {
			escape = true
			continue
		}
		if c == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		switch c {
		case '{', '[':
			depth++
			if depth > maxDepth {
				maxDepth = depth
			}
		case '}', ']':
			depth--
		}
	}
	return maxDepth
}
