package inbound

import (
	"os"
	"strings"
)

// readVerifyGoSource reads the package's verify.go source so the
// regression test [TestVerifySignature_ConstantTimeCompare] can pin
// the [hmac.Equal] choice. The path is relative to the test binary's
// working directory, which `go test` sets to the package dir — so
// `verify.go` resolves directly.
func readVerifyGoSource() (string, error) {
	b, err := os.ReadFile("verify.go")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// contains is a thin wrapper over [strings.Contains] used by the
// constant-time regression test. Hoisted so the test reads like English
// without importing strings at every assertion.
func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
