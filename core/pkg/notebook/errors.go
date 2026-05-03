package notebook

import "errors"

// ErrInvalidEntry is returned by [DB.Remember], [DB.Recall], and [DB.Forget]
// when the supplied input fails pre-DB validation: an empty `Content`, a
// `Category` outside the fixed enum, an `Embedding` whose length is not
// [EmbeddingDim], or a non-canonical UUID id. Callers should treat this as a
// programmer error — the request never reaches SQLite.
var ErrInvalidEntry = errors.New("notebook: invalid entry")

// ErrNotFound is returned by [DB.Forget] when the supplied id is well-formed
// but no row in `entry` matches. The transaction is rolled back; the caller
// can retry with a different id.
var ErrNotFound = errors.New("notebook: entry not found")
