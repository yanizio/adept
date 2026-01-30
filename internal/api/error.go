// internal/api/error.go
//
// Error wrappers for API requests.

package api

import "fmt"

// Error captures HTTP failures.
type Error struct {
	StatusCode int
	Body       string
}

func (e Error) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("api: request failed with status %d", e.StatusCode)
	}
	return fmt.Sprintf("api: request failed with status %d: %s", e.StatusCode, e.Body)
}
