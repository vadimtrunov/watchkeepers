package hostedexport_test

import (
	"context"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/hostedexport"
	"github.com/vadimtrunov/watchkeepers/core/pkg/toolregistry"
)

// fakeFS is a hand-rolled in-memory [hostedexport.FS] mirroring the
// `localpatch_test` fakeFS shape. Files keyed by clean absolute
// path; directories implicit via prefix matching against entries.
type fakeFS struct {
	mu      sync.Mutex
	files   map[string]fakeEntry
	dirs    map[string]bool
	statErr map[string]error
	readErr map[string]error
}

type fakeEntry struct {
	content    []byte
	mode       fs.FileMode
	executable bool
}

func newFakeFS() *fakeFS {
	return &fakeFS{
		files:   map[string]fakeEntry{},
		dirs:    map[string]bool{},
		statErr: map[string]error{},
		readErr: map[string]error{},
	}
}

func (f *fakeFS) AddFile(path string, content []byte) {
	f.AddFileMode(path, content, 0o644)
}

func (f *fakeFS) AddFileMode(path string, content []byte, mode fs.FileMode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	clean := filepath.Clean(path)
	exec := mode&0o111 != 0
	f.files[clean] = fakeEntry{content: content, mode: mode, executable: exec}
	dir := filepath.Dir(clean)
	for dir != "." && dir != "/" {
		f.dirs[dir] = true
		dir = filepath.Dir(dir)
	}
}

func (f *fakeFS) AddDir(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirs[filepath.Clean(path)] = true
}

func (f *fakeFS) MkdirAll(path string, _ fs.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	clean := filepath.Clean(path)
	for clean != "." && clean != "/" {
		f.dirs[clean] = true
		clean = filepath.Dir(clean)
	}
	return nil
}

func (f *fakeFS) ReadDir(path string) ([]fs.DirEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	clean := filepath.Clean(path)
	if err, ok := f.readErr[clean]; ok {
		return nil, err
	}
	if !f.dirs[clean] {
		return nil, &fs.PathError{Op: "readdir", Path: path, Err: fs.ErrNotExist}
	}
	seen := map[string]fs.DirEntry{}
	for p, e := range f.files {
		if filepath.Dir(p) == clean {
			seen[filepath.Base(p)] = &fakeDirEntry{name: filepath.Base(p), file: true, mode: e.mode}
		}
	}
	for d := range f.dirs {
		if filepath.Dir(d) == clean && d != clean {
			seen[filepath.Base(d)] = &fakeDirEntry{name: filepath.Base(d), file: false}
		}
	}
	out := make([]fs.DirEntry, 0, len(seen))
	for _, e := range seen {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

func (f *fakeFS) ReadFile(path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	clean := filepath.Clean(path)
	if err, ok := f.readErr[clean]; ok {
		return nil, err
	}
	e, ok := f.files[clean]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
	}
	return append([]byte(nil), e.content...), nil
}

func (f *fakeFS) Stat(path string) (fs.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	clean := filepath.Clean(path)
	if err, ok := f.statErr[clean]; ok {
		return nil, err
	}
	if e, ok := f.files[clean]; ok {
		return &fakeFileInfo{name: filepath.Base(clean), size: int64(len(e.content)), mode: e.mode}, nil
	}
	if f.dirs[clean] {
		return &fakeFileInfo{name: filepath.Base(clean), mode: fs.ModeDir | 0o755, isDir: true}, nil
	}
	return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}
}

func (f *fakeFS) Lstat(path string) (fs.FileInfo, error) {
	return f.Stat(path)
}

func (f *fakeFS) WriteFile(path string, data []byte, perm fs.FileMode) error {
	f.AddFileMode(path, data, perm)
	return nil
}

func (f *fakeFS) RemoveAll(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	clean := filepath.Clean(path)
	for p := range f.files {
		if p == clean || strings.HasPrefix(p, clean+string(filepath.Separator)) {
			delete(f.files, p)
		}
	}
	for d := range f.dirs {
		if d == clean || strings.HasPrefix(d, clean+string(filepath.Separator)) {
			delete(f.dirs, d)
		}
	}
	return nil
}

type fakeDirEntry struct {
	name string
	file bool
	mode fs.FileMode
}

func (e *fakeDirEntry) Name() string { return e.name }
func (e *fakeDirEntry) IsDir() bool  { return !e.file }
func (e *fakeDirEntry) Type() fs.FileMode {
	if !e.file {
		return fs.ModeDir
	}
	return 0
}

func (e *fakeDirEntry) Info() (fs.FileInfo, error) {
	return &fakeFileInfo{name: e.name, mode: e.mode, isDir: !e.file}, nil
}

type fakeFileInfo struct {
	name  string
	size  int64
	mode  fs.FileMode
	isDir bool
}

func (i *fakeFileInfo) Name() string       { return i.name }
func (i *fakeFileInfo) Size() int64        { return i.size }
func (i *fakeFileInfo) Mode() fs.FileMode  { return i.mode }
func (i *fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (i *fakeFileInfo) IsDir() bool        { return i.isDir }
func (i *fakeFileInfo) Sys() any           { return nil }

// fakePublisher captures every Publish call for assertions.
type fakePublisher struct {
	mu     sync.Mutex
	calls  []publishCall
	err    error
	ctxErr error
}

type publishCall struct {
	ctx   context.Context
	topic string
	event any
}

func (p *fakePublisher) Publish(ctx context.Context, topic string, event any) error {
	if p.ctxErr != nil {
		return p.ctxErr
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, publishCall{ctx: ctx, topic: topic, event: event})
	return p.err
}

func (p *fakePublisher) snapshot() []publishCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]publishCall, len(p.calls))
	copy(out, p.calls)
	return out
}

// fakeClock pins a deterministic value with optional per-call
// advancement.
type fakeClock struct {
	mu    sync.Mutex
	t     time.Time
	delta time.Duration
}

func newFakeClock(t time.Time) *fakeClock {
	return &fakeClock{t: t}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.t
	c.t = c.t.Add(c.delta)
	return now
}

// fakeLogger captures every Log call.
type fakeLogger struct {
	mu      sync.Mutex
	entries []fakeLogEntry
}

type fakeLogEntry struct {
	msg string
	kv  []any
}

func (l *fakeLogger) Log(_ context.Context, msg string, kv ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, fakeLogEntry{msg: msg, kv: append([]any(nil), kv...)})
}

func (l *fakeLogger) snapshot() []fakeLogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]fakeLogEntry, len(l.entries))
	copy(out, l.entries)
	return out
}

// constSourceLookup returns a single canned [toolregistry.SourceConfig].
func constSourceLookup(cfg toolregistry.SourceConfig, err error) hostedexport.SourceLookup {
	return func(_ context.Context, _ string) (toolregistry.SourceConfig, error) {
		return cfg, err
	}
}

// constOperatorResolver returns a canned resolved id / error.
func constOperatorResolver(id string, err error) hostedexport.OperatorIdentityResolver {
	return func(_ context.Context, _ string) (string, error) {
		return id, err
	}
}

// validHostedSource returns a canned `kind: hosted` [toolregistry.SourceConfig].
func validHostedSource(name string) toolregistry.SourceConfig {
	return toolregistry.SourceConfig{
		Name:       name,
		Kind:       toolregistry.SourceKindHosted,
		PullPolicy: toolregistry.PullPolicyOnDemand,
	}
}

// validLocalSource returns a canned `kind: local` [toolregistry.SourceConfig]
// used in the source-kind-rejection test.
func validLocalSource(name string) toolregistry.SourceConfig {
	return toolregistry.SourceConfig{
		Name:       name,
		Kind:       toolregistry.SourceKindLocal,
		PullPolicy: toolregistry.PullPolicyOnDemand,
	}
}

// validManifestJSON renders a [toolregistry.Manifest] JSON body with
// the bare-minimum required fields for [toolregistry.DecodeManifest].
func validManifestJSON(name, version string) []byte {
	return []byte(`{"name":"` + name +
		`","version":"` + version +
		`","capabilities":["read:logs"],"schema":{"type":"object"},"dry_run_mode":"none"}`)
}
