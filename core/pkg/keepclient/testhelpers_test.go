package keepclient

import (
	"net/http"
	"time"
)

// newTestHTTPClient returns a fresh *http.Client with the supplied timeout.
// Test-only helper so individual cases do not duplicate the constructor.
func newTestHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout}
}
