package notebook

import (
	"time"
)

// Category enumerates the five fixed entry categories enforced by the
// `entry.category` CHECK constraint in [schemaSQL]. Mirrors the M2b.1 schema
// so client-side validation and server-side enforcement reject the same set.
const (
	CategoryLesson           = "lesson"
	CategoryPreference       = "preference"
	CategoryObservation      = "observation"
	CategoryPendingTask      = "pending_task"
	CategoryRelationshipNote = "relationship_note"
)

// categoryEnum is the closed set of allowed [Entry.Category] values. Used by
// [validate] to reject unknown categories before the DB CHECK constraint
// would. Kept private so the schema constant remains the single source of
// truth.
var categoryEnum = map[string]struct{}{
	CategoryLesson:           {},
	CategoryPreference:       {},
	CategoryObservation:      {},
	CategoryPendingTask:      {},
	CategoryRelationshipNote: {},
}

// maxTopK clamps the [RecallQuery.TopK] server-side. sqlite-vec evaluates KNN
// against every row in the virtual table; an unbounded TopK lets a caller
// trivially exhaust a notebook. 100 is the per-call ceiling agreed in AC2.
const maxTopK = 100

// Entry is the in-memory representation of a single Notebook row. Mirrors the
// columns defined by [schemaSQL] one-to-one. `Embedding` is the float32
// vector stored in the sibling `entry_vec` virtual table; the [DB.Remember]
// path serialises it via the sqlite-vec binding before insert.
//
// Pointer fields (`LastUsedAt`, `RelevanceScore`, `SupersededBy`,
// `EvidenceLogRef`, `ToolVersion`) carry SQL NULL semantics: nil means the
// column is unset. `Subject` uses a value type because the M2b.1 schema
// allows NULL for it but the API treats the empty string and NULL as
// equivalent — empty subjects are stored as NULL.
type Entry struct {
	// ID is the canonical UUID v7 string PK. When empty on [DB.Remember],
	// the implementation auto-generates one and returns it.
	ID string

	// Category is one of the five [categoryEnum] values. Required.
	Category string

	// Subject is an optional human-readable subject (e.g. an entity name).
	// Empty string is stored as NULL.
	Subject string

	// Content is the textual body of the entry. Required, non-empty.
	Content string

	// CreatedAt is the unix epoch millisecond at which the entry was first
	// recorded. When zero on [DB.Remember], the implementation defaults to
	// `time.Now().UnixMilli()`.
	CreatedAt int64

	// LastUsedAt is the unix epoch millisecond of the most recent recall hit
	// against this entry. Optional.
	LastUsedAt *int64

	// RelevanceScore is an optional float in [0, 1] used by ranking layers
	// above this package.
	RelevanceScore *float64

	// SupersededBy, when set, is the id of a newer entry that supersedes
	// this one. Recall filters out rows where this is non-NULL.
	SupersededBy *string

	// EvidenceLogRef is an optional foreign reference to an external
	// evidence log entry (see M2b.7).
	EvidenceLogRef *string

	// ToolVersion is the optional semantic version of the tool that produced
	// the entry, for forensic correlation.
	ToolVersion *string

	// ActiveAfter is a unix epoch millisecond before which Recall must not
	// surface the entry (default 0 = always active).
	ActiveAfter int64

	// Embedding is the dense float32 vector stored in `entry_vec`. Required
	// on [DB.Remember]; length must equal [EmbeddingDim].
	Embedding []float32
}

// RecallQuery is the input shape for [DB.Recall]. `Embedding` is the query
// vector; `TopK` is the maximum number of nearest neighbours to return,
// server-clamped to [maxTopK]. `Category`, when non-empty, restricts the
// result to a single category. `ActiveAt`, when non-zero, restricts the
// result to rows whose `active_after <= ActiveAt.UnixMilli()`; the zero
// value defaults to `time.Now()`.
type RecallQuery struct {
	Embedding []float32
	TopK      int
	Category  string
	ActiveAt  time.Time
}

// RecallResult is one row returned by [DB.Recall]. Mirrors the queryable
// columns of `entry` plus the cosine `Distance` reported by sqlite-vec.
// Rows are returned ordered by ascending `Distance`.
type RecallResult struct {
	ID             string
	Category       string
	Subject        string
	Content        string
	CreatedAt      int64
	LastUsedAt     *int64
	RelevanceScore *float64
	EvidenceLogRef *string
	ToolVersion    *string
	ActiveAfter    int64
	Distance       float64
}

// Stats is the aggregate counts returned by [DB.Stats]. `TotalEntries` is the
// raw row count; `Active` is `superseded_by IS NULL AND active_after <= now`;
// `Superseded` is `superseded_by IS NOT NULL`. `ByCategory` maps each of the
// five [categoryEnum] values to the count of *active* entries in that
// category.
type Stats struct {
	TotalEntries int
	Active       int
	Superseded   int
	ByCategory   map[string]int
}

// validate runs the pre-DB shape checks for [DB.Remember]. Returns
// [ErrInvalidEntry] on any failure; the caller must propagate it without
// touching the database.
func validate(e *Entry) error {
	if e.Content == "" {
		return ErrInvalidEntry
	}
	if _, ok := categoryEnum[e.Category]; !ok {
		return ErrInvalidEntry
	}
	if len(e.Embedding) != EmbeddingDim {
		return ErrInvalidEntry
	}
	return nil
}
