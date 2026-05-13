package localpatch

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/toolregistry"
)

// fakeFS is a hand-rolled in-memory [FS]. Tests construct trees via
// [fakeFS.AddFile] (which auto-creates parent dirs), inspect call
// counters, and seed per-path errors via the `*Err` maps. Concurrency-
// safe — the install/rollback tests fire 16 goroutines through the
// same FS to exercise the per-call seam discipline.
type fakeFS struct {
	mu sync.Mutex

	files     map[string][]byte
	fileModes map[string]fs.FileMode
	dirs      map[string]bool

	mkdirErr     map[string]error
	statErr      map[string]error
	readErr      map[string]error
	readDirErr   map[string]error
	writeErr     map[string]error
	removeAllErr map[string]error

	mkdirCalls     int
	statCalls      int
	readCalls      int
	readDirCalls   int
	writeCalls     int
	removeAllCalls int

	// onWrite, when non-nil, fires AFTER each successful WriteFile.
	// Used by [TestInstall_PublishUsesContextWithoutCancel] to cancel
	// the parent ctx DURING a side-effect — the assertion then
	// verifies the publish below uses [context.WithoutCancel] rather
	// than the parent ctx.
	onWrite func(path string)
}

func newFakeFS() *fakeFS {
	return &fakeFS{
		files:        map[string][]byte{},
		fileModes:    map[string]fs.FileMode{},
		dirs:         map[string]bool{},
		mkdirErr:     map[string]error{},
		statErr:      map[string]error{},
		readErr:      map[string]error{},
		readDirErr:   map[string]error{},
		writeErr:     map[string]error{},
		removeAllErr: map[string]error{},
	}
}

// AddFile populates a regular file at `path` with `content` and
// auto-creates parent directories (mirrors `os.MkdirAll(parent)` +
// `os.WriteFile(path)`). The default mode is 0o644; pass an explicit
// mode via [fakeFS.AddFileMode] when an executable bit matters.
func (f *fakeFS) AddFile(path string, content []byte) {
	f.AddFileMode(path, content, 0o644)
}

// AddFileMode is the mode-aware twin of [fakeFS.AddFile].
func (f *fakeFS) AddFileMode(path string, content []byte, mode fs.FileMode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cleaned := filepath.Clean(path)
	cp := make([]byte, len(content))
	copy(cp, content)
	f.files[cleaned] = cp
	f.fileModes[cleaned] = mode
	for d := filepath.Dir(cleaned); d != "" && d != "." && d != string(filepath.Separator); d = filepath.Dir(d) {
		f.dirs[d] = true
	}
}

// AddDir registers a directory without any file content.
func (f *fakeFS) AddDir(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirs[filepath.Clean(path)] = true
}

func (f *fakeFS) MkdirAll(path string, _ fs.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mkdirCalls++
	if err, ok := f.mkdirErr[path]; ok {
		return err
	}
	cleaned := filepath.Clean(path)
	for d := cleaned; d != "" && d != "." && d != string(filepath.Separator); d = filepath.Dir(d) {
		f.dirs[d] = true
	}
	return nil
}

func (f *fakeFS) Stat(path string) (fs.FileInfo, error) {
	return f.statShared("stat", path)
}

// Lstat shares semantics with Stat in the fake — there are no
// symlinks in the in-memory FS so following / not-following collapses
// to the same record. Real-FS symlink behaviour is exercised in the
// CLI test (`run_test.go`) against `t.TempDir()`.
func (f *fakeFS) Lstat(path string) (fs.FileInfo, error) {
	return f.statShared("lstat", path)
}

func (f *fakeFS) statShared(op, path string) (fs.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statCalls++
	if err, ok := f.statErr[path]; ok {
		return nil, err
	}
	cleaned := filepath.Clean(path)
	if f.dirs[cleaned] {
		return fakeInfo{name: filepath.Base(cleaned), isDir: true, mode: fs.ModeDir | 0o755}, nil
	}
	if data, ok := f.files[cleaned]; ok {
		return fakeInfo{name: filepath.Base(cleaned), size: int64(len(data)), mode: f.fileModes[cleaned]}, nil
	}
	return nil, &fs.PathError{Op: op, Path: path, Err: fs.ErrNotExist}
}

func (f *fakeFS) ReadFile(path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readCalls++
	if err, ok := f.readErr[path]; ok {
		return nil, err
	}
	cleaned := filepath.Clean(path)
	data, ok := f.files[cleaned]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
	}
	out := make([]byte, len(data))
	copy(out, data)
	return out, nil
}

func (f *fakeFS) ReadDir(path string) ([]fs.DirEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readDirCalls++
	if err, ok := f.readDirErr[path]; ok {
		return nil, err
	}
	cleaned := filepath.Clean(path)
	if !f.dirs[cleaned] {
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
	}
	seen := map[string]fakeDirEntry{}
	prefix := cleaned + string(filepath.Separator)
	for fpath, mode := range f.fileModes {
		if !strings.HasPrefix(fpath, prefix) {
			continue
		}
		rest := fpath[len(prefix):]
		first := rest
		if idx := strings.Index(rest, string(filepath.Separator)); idx >= 0 {
			first = rest[:idx]
			seen[first] = fakeDirEntry{name: first, isDir: true, mode: fs.ModeDir | 0o755}
		} else {
			seen[first] = fakeDirEntry{name: first, isDir: false, mode: mode}
		}
	}
	for d := range f.dirs {
		if !strings.HasPrefix(d, prefix) {
			continue
		}
		rest := d[len(prefix):]
		if idx := strings.Index(rest, string(filepath.Separator)); idx >= 0 {
			first := rest[:idx]
			if _, ok := seen[first]; !ok {
				seen[first] = fakeDirEntry{name: first, isDir: true, mode: fs.ModeDir | 0o755}
			}
		} else if _, ok := seen[rest]; !ok {
			seen[rest] = fakeDirEntry{name: rest, isDir: true, mode: fs.ModeDir | 0o755}
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]fs.DirEntry, 0, len(names))
	for _, n := range names {
		out = append(out, seen[n])
	}
	return out, nil
}

func (f *fakeFS) WriteFile(path string, data []byte, perm fs.FileMode) error {
	f.mu.Lock()
	f.writeCalls++
	if err, ok := f.writeErr[path]; ok {
		f.mu.Unlock()
		return err
	}
	cleaned := filepath.Clean(path)
	cp := make([]byte, len(data))
	copy(cp, data)
	f.files[cleaned] = cp
	f.fileModes[cleaned] = perm
	for d := filepath.Dir(cleaned); d != "" && d != "." && d != string(filepath.Separator); d = filepath.Dir(d) {
		f.dirs[d] = true
	}
	hook := f.onWrite
	f.mu.Unlock()
	if hook != nil {
		hook(cleaned)
	}
	return nil
}

func (f *fakeFS) RemoveAll(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeAllCalls++
	if err, ok := f.removeAllErr[path]; ok {
		return err
	}
	cleaned := filepath.Clean(path)
	prefix := cleaned + string(filepath.Separator)
	for fpath := range f.files {
		if fpath == cleaned || strings.HasPrefix(fpath, prefix) {
			delete(f.files, fpath)
			delete(f.fileModes, fpath)
		}
	}
	for d := range f.dirs {
		if d == cleaned || strings.HasPrefix(d, prefix) {
			delete(f.dirs, d)
		}
	}
	return nil
}

// hasFile reports whether `path` exists in the fake. Mirrors the
// helpers in `toolregistry`'s fakes.
func (f *fakeFS) hasFile(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.files[filepath.Clean(path)]
	return ok
}

// hasDir reports whether `path` is registered as a directory.
func (f *fakeFS) hasDir(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dirs[filepath.Clean(path)]
}

// fakeDirEntry is a controllable [fs.DirEntry].
type fakeDirEntry struct {
	name  string
	isDir bool
	mode  fs.FileMode
}

func (e fakeDirEntry) Name() string      { return e.name }
func (e fakeDirEntry) IsDir() bool       { return e.isDir }
func (e fakeDirEntry) Type() fs.FileMode { return e.mode.Type() }
func (e fakeDirEntry) Info() (fs.FileInfo, error) {
	return fakeInfo{name: e.name, isDir: e.isDir, mode: e.mode}, nil
}

// fakeInfo is the [fs.FileInfo] returned by [fakeFS.Stat].
type fakeInfo struct {
	name  string
	size  int64
	mode  fs.FileMode
	isDir bool
}

func (i fakeInfo) Name() string       { return i.name }
func (i fakeInfo) Size() int64        { return i.size }
func (i fakeInfo) Mode() fs.FileMode  { return i.mode }
func (i fakeInfo) ModTime() time.Time { return time.Time{} }
func (i fakeInfo) IsDir() bool        { return i.isDir || i.mode.IsDir() }
func (i fakeInfo) Sys() any           { return nil }

// fakePublisher records every Publish call. The `publishErr` field
// lets tests simulate eventbus failures.
type fakePublisher struct {
	mu sync.Mutex

	publishErr error

	events []publishedEvent
}

type publishedEvent struct {
	topic string
	event any
}

func (p *fakePublisher) Publish(_ context.Context, topic string, event any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, publishedEvent{topic: topic, event: event})
	return p.publishErr
}

func (p *fakePublisher) snapshot() []publishedEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]publishedEvent, len(p.events))
	copy(out, p.events)
	return out
}

// fakeClock returns a fixed time. Tests advance it via SetNow to
// simulate distinct correlation ids across operations.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{now: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) SetNow(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// fakeLogger records every Log call.
type fakeLogger struct {
	mu sync.Mutex

	entries []loggedEntry
}

type loggedEntry struct {
	msg string
	kv  []any
}

func (l *fakeLogger) Log(_ context.Context, msg string, kv ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cp := make([]any, len(kv))
	copy(cp, kv)
	l.entries = append(l.entries, loggedEntry{msg: msg, kv: cp})
}

func (l *fakeLogger) snapshot() []loggedEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]loggedEntry, len(l.entries))
	copy(out, l.entries)
	return out
}

// constSourceLookup returns a fixed [toolregistry.SourceConfig] for
// any matching name; an unmatched name yields [ErrUnknownSource].
type constSourceLookup struct {
	configs map[string]toolregistry.SourceConfig
}

func newConstSourceLookup(cfgs ...toolregistry.SourceConfig) *constSourceLookup {
	m := make(map[string]toolregistry.SourceConfig, len(cfgs))
	for _, c := range cfgs {
		m[c.Name] = c
	}
	return &constSourceLookup{configs: m}
}

func (l *constSourceLookup) Lookup(_ context.Context, name string) (toolregistry.SourceConfig, error) {
	cfg, ok := l.configs[name]
	if !ok {
		return toolregistry.SourceConfig{}, errFakeUnknownSource
	}
	return cfg, nil
}

// errFakeUnknownSource is the test-only sentinel returned by
// [constSourceLookup.Lookup] when the name does not resolve. The
// installer wraps it with [ErrUnknownSource]; the test asserts both
// sentinels via `errors.Is`.
var errFakeUnknownSource = errors.New("fake: unknown source")

// constOperatorResolver returns a fixed operator id. Tests inject an
// `errOnHint` to surface an error when a specific hint is presented.
type constOperatorResolver struct {
	id        string
	err       error
	hintCalls []string
	mu        sync.Mutex
}

func (r *constOperatorResolver) Resolve(_ context.Context, hint string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hintCalls = append(r.hintCalls, hint)
	return r.id, r.err
}

// validLocalSource returns a [toolregistry.SourceConfig] of kind
// `local` with the supplied name. Helpers use this so each test
// stays one-line for the source-lookup fake.
func validLocalSource(name string) toolregistry.SourceConfig {
	return toolregistry.SourceConfig{
		Name:       name,
		Kind:       toolregistry.SourceKindLocal,
		PullPolicy: toolregistry.PullPolicyOnDemand,
	}
}

// validGitSource returns a non-local source for the
// ErrInvalidSourceKind path.
func validGitSource(name string) toolregistry.SourceConfig {
	return toolregistry.SourceConfig{
		Name:       name,
		Kind:       toolregistry.SourceKindGit,
		URL:        "https://example.test/repo.git",
		PullPolicy: toolregistry.PullPolicyOnBoot,
	}
}

// validManifestJSON returns a minimal valid manifest body for the
// tool-folder fixtures. Mirrors `toolregistry`'s test-helper style
// (see `core/pkg/toolregistry/testhelpers_test.go`).
func validManifestJSON(name, version string) []byte {
	return []byte(`{
"name":"` + name + `",
"version":"` + version + `",
"capabilities":["cap.test"],
"schema":{"type":"object"},
"dry_run_mode":"none"
}`)
}
