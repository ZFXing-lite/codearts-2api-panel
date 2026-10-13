package api

import "net/http"

// decodeJSONBody applies the shared 16 MiB body limit and nesting-depth check.
func decodeJSONBody(r *http.Request, v any) error {
	return newLimitedDecoder(r.Body).Decode(v)
}
