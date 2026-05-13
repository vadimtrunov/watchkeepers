package github

import "fmt"

// refContentBytesValid checks that `ref` contains no bytes that
// would either corrupt the composed URL or surface as confusing
// server-side errors. Rejected bytes: ASCII control chars (0x00-
// 0x1F + 0x7F), whitespace (' ', '\t', '\n', '\r'), and the URL-
// metacharacters `?`, `#`, `%`.
//
// Shared by [validateRefRelative] (GET path) and [validateRefFull]
// (POST path). Iter-1 fixes (reviewer A M3 + m4, reviewer B M2 +
// m9): unified content-byte allowlist defends both call sites
// from the same byte-level corruption risk.
func refContentBytesValid(ref string) error {
	for i := 0; i < len(ref); i++ {
		c := ref[i]
		switch {
		case c < 0x20:
			return fmt.Errorf("%w: ref control byte 0x%02x at %d", ErrInvalidArgs, c, i)
		case c == 0x7F:
			return fmt.Errorf("%w: ref DEL byte at %d", ErrInvalidArgs, i)
		case c == ' ', c == '\t':
			return fmt.Errorf("%w: ref whitespace at %d", ErrInvalidArgs, i)
		case c == '?', c == '#', c == '%':
			return fmt.Errorf("%w: ref URL metachar %q at %d", ErrInvalidArgs, c, i)
		}
	}
	return nil
}
