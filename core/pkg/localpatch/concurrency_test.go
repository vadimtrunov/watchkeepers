package localpatch

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

// TestInstall_16GoroutineConcurrency exercises 16 goroutines hitting
// distinct (source, tool) targets through the same [Installer]. The
// installer is safe-for-concurrent-use AS LONG AS each invocation
// targets a distinct (source, tool) pair (mirrors `toolregistry.Scheduler`'s
// per-source mutex contract — same-source operations serialise via
// the operator's CLI invocation, not via the runtime). The test
// asserts: every install succeeds; all 16 events land on the bus;
// no panic / data race surfaces under -race.
func TestInstall_16GoroutineConcurrency(t *testing.T) {
	t.Parallel()
	const n = 16
	fx := newInstallerFixture(t)
	// Pre-stage 16 distinct tool folders.
	tools := make([]string, n)
	for i := 0; i < n; i++ {
		tool := "tool_" + suffix(i)
		tools[i] = tool
		path := "/staging/" + tool
		fx.fs.AddFile(filepath.Join(path, "manifest.json"), validManifestJSON(tool, "1.0.0"))
		fx.fs.AddFile(filepath.Join(path, "src/x.ts"), []byte(`x`))
	}

	var wg sync.WaitGroup
	wg.Add(n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			req := InstallRequest{
				SourceName:     testSourceName,
				FolderPath:     "/staging/" + tools[i],
				Reason:         "concurrent test " + suffix(i),
				OperatorIDHint: testOperatorID,
			}
			if _, err := fx.inst.Install(context.Background(), req); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent install: %v", err)
	}
	if got := len(fx.pub.snapshot()); got != n {
		t.Errorf("event count: got %d want %d", got, n)
	}
}

// suffix returns a 2-digit zero-padded representation of `i` so the
// generated tool names sort lexicographically the same way they
// were spawned.
func suffix(i int) string {
	const digits = "0123456789"
	return string([]byte{digits[i/10], digits[i%10]})
}
