package toolshare_test

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/github"
	"github.com/vadimtrunov/watchkeepers/core/pkg/toolregistry"
	"github.com/vadimtrunov/watchkeepers/core/pkg/toolshare"
)

// ---- fakeFS ----

type fakeFS struct {
	mu    sync.Mutex
	files map[string]fakeEntry
	dirs  map[string]bool
}

type fakeEntry struct {
	content []byte
	mode    fs.FileMode
}

func newFakeFS() *fakeFS {
	return &fakeFS{files: map[string]fakeEntry{}, dirs: map[string]bool{}}
}

func (f *fakeFS) AddFile(path string, content []byte) {
	f.AddFileMode(path, content, 0o644)
}

func (f *fakeFS) AddFileMode(path string, content []byte, mode fs.FileMode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	clean := filepath.Clean(path)
	f.files[clean] = fakeEntry{content: content, mode: mode}
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
	if e, ok := f.files[clean]; ok {
		return &fakeFileInfo{name: filepath.Base(clean), size: int64(len(e.content)), mode: e.mode}, nil
	}
	if f.dirs[clean] {
		return &fakeFileInfo{name: filepath.Base(clean), mode: fs.ModeDir | 0o755, isDir: true}, nil
	}
	return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}
}

func (f *fakeFS) Lstat(path string) (fs.FileInfo, error) { return f.Stat(path) }

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

// ---- fakePublisher ----

type fakePublisher struct {
	mu     sync.Mutex
	calls  []publishCall
	errFor map[string]error
}

type publishCall struct {
	ctx   context.Context
	topic string
	event any
}

func (p *fakePublisher) Publish(ctx context.Context, topic string, event any) error {
	if p.errFor != nil {
		if err, ok := p.errFor[topic]; ok {
			return err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, publishCall{ctx: ctx, topic: topic, event: event})
	return nil
}

func (p *fakePublisher) snapshot() []publishCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]publishCall, len(p.calls))
	copy(out, p.calls)
	return out
}

// ---- fakeClock ----

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// ---- fakeLogger ----

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

// ---- fakeGitHubClient ----

type fakeGitHubClient struct {
	mu             sync.Mutex
	getRefRes      github.GetRefResult
	getRefErr      error
	createRefRes   github.CreateRefResult
	createRefErr   error
	createFileRes  github.CreateOrUpdateFileResult
	createFileErr  error
	createFileErrs map[string]error // path-specific override
	openPRRes      github.CreatePullRequestResult
	openPRErr      error
	calls          []githubCall
}

type githubCall struct {
	op   string
	args any
}

func (g *fakeGitHubClient) GetRef(_ context.Context, owner github.RepoOwner, repo github.RepoName, ref string) (github.GetRefResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, githubCall{op: "GetRef", args: struct {
		Owner github.RepoOwner
		Repo  github.RepoName
		Ref   string
	}{owner, repo, ref}})
	if g.getRefErr != nil {
		return github.GetRefResult{}, g.getRefErr
	}
	return g.getRefRes, nil
}

func (g *fakeGitHubClient) CreateRef(_ context.Context, opts github.CreateRefOptions) (github.CreateRefResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, githubCall{op: "CreateRef", args: opts})
	if g.createRefErr != nil {
		return github.CreateRefResult{}, g.createRefErr
	}
	return g.createRefRes, nil
}

func (g *fakeGitHubClient) CreateOrUpdateFile(_ context.Context, opts github.CreateOrUpdateFileOptions) (github.CreateOrUpdateFileResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, githubCall{op: "CreateOrUpdateFile", args: opts})
	if err, ok := g.createFileErrs[opts.Path]; ok {
		return github.CreateOrUpdateFileResult{}, err
	}
	if g.createFileErr != nil {
		return github.CreateOrUpdateFileResult{}, g.createFileErr
	}
	return g.createFileRes, nil
}

func (g *fakeGitHubClient) CreatePullRequest(_ context.Context, opts github.CreatePullRequestOptions) (github.CreatePullRequestResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, githubCall{op: "CreatePullRequest", args: opts})
	if g.openPRErr != nil {
		return github.CreatePullRequestResult{}, g.openPRErr
	}
	return g.openPRRes, nil
}

func (g *fakeGitHubClient) snapshot() []githubCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]githubCall, len(g.calls))
	copy(out, g.calls)
	return out
}

// ---- fakeSlack ----

type fakeSlack struct {
	mu         sync.Mutex
	openIMErr  error
	sendErr    error
	imChannels map[string]string
	sent       []slackSendCall
}

type slackSendCall struct {
	channelID string
	text      string
}

func (s *fakeSlack) OpenIMChannel(_ context.Context, userID string) (string, error) {
	if s.openIMErr != nil {
		return "", s.openIMErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.imChannels[userID]
	if !ok {
		ch = "D-" + userID
	}
	return ch, nil
}

func (s *fakeSlack) SendDMText(_ context.Context, channelID, text string) error {
	if s.sendErr != nil {
		return s.sendErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, slackSendCall{channelID: channelID, text: text})
	return nil
}

func (s *fakeSlack) snapshot() []slackSendCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]slackSendCall, len(s.sent))
	copy(out, s.sent)
	return out
}

// ---- resolver helpers ----

func constSourceLookup(cfg toolregistry.SourceConfig, err error) toolshare.SourceLookup {
	return func(_ context.Context, _ string) (toolregistry.SourceConfig, error) { return cfg, err }
}

func constProposerResolver(id string, err error) toolshare.ProposerIdentityResolver {
	return func(_ context.Context, _ string) (string, error) { return id, err }
}

func constTargetResolver(target toolshare.ResolvedTarget, err error) toolshare.TargetRepoResolver {
	return func(_ context.Context, _ toolshare.ShareRequest) (toolshare.ResolvedTarget, error) {
		return target, err
	}
}

func constLeadResolver(userID string, err error) toolshare.LeadResolver {
	return func(_ context.Context, _ toolshare.ResolvedTarget, _ toolshare.ShareRequest) (string, error) {
		return userID, err
	}
}

func validSource(name string) toolregistry.SourceConfig {
	return toolregistry.SourceConfig{
		Name:       name,
		Kind:       toolregistry.SourceKindHosted,
		PullPolicy: toolregistry.PullPolicyOnDemand,
	}
}

func validTarget() toolshare.ResolvedTarget {
	return toolshare.ResolvedTarget{
		Owner:  "watchkeepers",
		Repo:   "watchkeeper-tools",
		Base:   "main",
		Source: toolshare.TargetSourcePlatform,
	}
}

func validManifestJSON(name, version string) []byte {
	return []byte(`{"name":"` + name +
		`","version":"` + version +
		`","capabilities":["read:logs"],"schema":{"type":"object"},"dry_run_mode":"none"}`)
}

var errBoom = errors.New("toolshare-fake: boom")
