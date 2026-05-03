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

// ErrCorruptArchive is returned by [DB.Import] when the supplied archive
// fails the spool-and-validate step: the SQLite header bytes do not match,
// the expected `entry` / `entry_vec` tables are missing, or one of the two
// hot-path partial indexes (`entry_category_active`, `entry_active_after`)
// has not been carried over. The temp spool file is deleted before returning.
var ErrCorruptArchive = errors.New("notebook: corrupt archive")

// ErrTargetNotEmpty is returned by [DB.Import] when the live DB receiving
// the snapshot still has at least one row in `entry`. M2b.2.b strictly
// refuses overwrites; M2b.6's CLI may layer a `--force` flag that calls
// [DB.Archive] + Forget-all + [DB.Import], but this package itself never
// drops live data.
var ErrTargetNotEmpty = errors.New("notebook: import target not empty")
