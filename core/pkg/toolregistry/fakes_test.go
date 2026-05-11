package toolregistry

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"sync"
	"time"
)

// fakeFS is a hand-rolled in-memory [FS]. Tests populate `files` and
// `dirs` directly and observe the call counters (`statCalls`, etc.)
// to assert the scheduler's behaviour. Concurrency-safe via `mu`
// because some tests dispatch SyncOnce from multiple goroutines.
type fakeFS struct {
	mu sync.Mutex

	files map[string][]byte   // path → contents
	dirs  map[string]bool     // path → exists
	infos map[string]fakeInfo // optional: explicit FileInfo

	// dirEntries maps a parent directory path to the [fs.DirEntry] list
	// returned by [fakeFS.ReadDir]. Set entries explicitly per test;
	// leave unset to let ReadDir return fs.ErrNotExist for an unknown
	// parent (mirrors real-OS behaviour for a missing dir).
	dirEntries map[string][]fs.DirEntry

	mkdirErr   map[string]error // path → error to return
	statErr    map[string]error
	readErr    map[string]error
	readDirErr map[string]error

	statCalls    int
	mkdirCalls   int
	readCalls    int
	readDirCalls int
}

func newFakeFS() *fakeFS {
	return &fakeFS{
		files:      map[string][]byte{},
		dirs:       map[string]bool{},
		infos:      map[string]fakeInfo{},
		dirEntries: map[string][]fs.DirEntry{},
		mkdirErr:   map[string]error{},
		statErr:    map[string]error{},
		readErr:    map[string]error{},
		readDirErr: map[string]error{},
	}
}

func (f *fakeFS) MkdirAll(path string, _ os.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mkdirCalls++
	if err, ok := f.mkdirErr[path]; ok {
		return err
	}
	f.dirs[path] = true
	return nil
}

func (f *fakeFS) Stat(path string) (os.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statCalls++
	if err, ok := f.statErr[path]; ok {
		return nil, err
	}
	if info, ok := f.infos[path]; ok {
		return info, nil
	}
	if f.dirs[path] {
		return fakeInfo{name: path, isDir: true}, nil
	}
	if _, ok := f.files[path]; ok {
		return fakeInfo{name: path}, nil
	}
	return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}
}

func (f *fakeFS) ReadFile(path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readCalls++
	if err, ok := f.readErr[path]; ok {
		return nil, err
	}
	b, ok := f.files[path]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
	}
	// Return a fresh copy so caller mutation never bleeds back.
	out := make([]byte, len(b))
	copy(out, b)
	return out, nil
}

func (f *fakeFS) ReadDir(path string) ([]fs.DirEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readDirCalls++
	if err, ok := f.readDirErr[path]; ok {
		return nil, err
	}
	entries, ok := f.dirEntries[path]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
	}
	// Return a fresh slice so caller mutation never bleeds back.
	out := make([]fs.DirEntry, len(entries))
	copy(out, entries)
	return out, nil
}

// fakeDirEntry is a controllable [fs.DirEntry] used by tests to
// populate [fakeFS.dirEntries] without depending on a real OS
// directory. Only the methods the M9.1.b scanner consults are
// implemented; [fakeDirEntry.Info] returns ([fs.FileInfo](nil), nil)
// because the scanner does not call it.
type fakeDirEntry struct {
	name  string
	isDir bool
}

func (e fakeDirEntry) Name() string               { return e.name }
func (e fakeDirEntry) IsDir() bool                { return e.isDir }
func (e fakeDirEntry) Type() fs.FileMode          { return 0 }
func (e fakeDirEntry) Info() (fs.FileInfo, error) { return nil, nil }

// fakeInfo is the [os.FileInfo] returned by [fakeFS.Stat]. Only the
// fields the scheduler actually consults are populated.
type fakeInfo struct {
	name  string
	isDir bool
}

func (f fakeInfo) Name() string       { return f.name }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() os.FileMode  { return 0 }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.isDir }
func (f fakeInfo) Sys() any           { return nil }

// fakeGit is a hand-rolled [GitClient]. Tests configure per-call
// behaviour via the `clone*` / `pull*` fields; the counters let
// tests assert call counts and per-call arguments.
type fakeGit struct {
	mu sync.Mutex

	cloneErr error
	pullErr  error

	cloneCalls []CloneOpts
	pullCalls  []PullOpts

	// onClone / onPull optionally side-effect (e.g. mark a
	// `.git` directory present so the next SyncOnce takes the
	// pull path).
	onClone func(opts CloneOpts)
	onPull  func(opts PullOpts)
}

func (g *fakeGit) Clone(_ context.Context, opts CloneOpts) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cloneCalls = append(g.cloneCalls, opts)
	if g.onClone != nil {
		g.onClone(opts)
	}
	return g.cloneErr
}

func (g *fakeGit) Pull(_ context.Context, opts PullOpts) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pullCalls = append(g.pullCalls, opts)
	if g.onPull != nil {
		g.onPull(opts)
	}
	return g.pullErr
}

func (g *fakeGit) numCloneCalls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.cloneCalls)
}

func (g *fakeGit) numPullCalls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.pullCalls)
}

func (g *fakeGit) lastClone() (CloneOpts, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.cloneCalls) == 0 {
		return CloneOpts{}, false
	}
	return g.cloneCalls[len(g.cloneCalls)-1], true
}

func (g *fakeGit) lastPull() (PullOpts, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.pullCalls) == 0 {
		return PullOpts{}, false
	}
	return g.pullCalls[len(g.pullCalls)-1], true
}

// fakeClock returns a fixed time. Tests can advance it via `advance`
// to simulate distinct correlation ids across SyncOnce calls.
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

// fakePublisher records every Publish call. The `publishErr` field
// lets tests simulate eventbus failures; the recorder lets tests
// assert the exact topic + payload + sequence.
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

func (p *fakePublisher) eventsForTopic(topic string) []publishedEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []publishedEvent
	for _, e := range p.events {
		if e.topic == topic {
			out = append(out, e)
		}
	}
	return out
}

// fakeVerifier is a controllable [SignatureVerifier]. The `verifyErr`
// field surfaces an error on Verify; the `calls` slice records the
// (sourceName, dir) arguments.
type fakeVerifier struct {
	mu sync.Mutex

	verifyErr error
	calls     []verifierCall
}

type verifierCall struct {
	sourceName string
	dir        string
}

func (v *fakeVerifier) Verify(_ context.Context, sourceName, dir string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls = append(v.calls, verifierCall{sourceName: sourceName, dir: dir})
	return v.verifyErr
}

func (v *fakeVerifier) numCalls() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.calls)
}

// fakeLogger records every Log call so tests can assert that the
// canary auth-token never appears in any logged kv pair.
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
	// Copy the variadic slice so callers reusing a backing array
	// cannot mutate our record post-Log.
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

// errSentinel is a tiny test-only sentinel for fakeGit / fakeFS to
// return; isolates the test signal from sentinels in production
// code so an accidental aliasing bug surfaces immediately.
var errSentinel = errors.New("fake error sentinel")
