package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type fixtureRun struct {
	code            int
	summary, runDir string
	full, compact   string
}

// runFixture runs ailog in-process against the fixture module with a real go test child.
func runFixture(t *testing.T, create func(dir, name string) (io.WriteCloser, error), args ...string) fixtureRun {
	t.Helper()
	integration(t)
	return runFixtureIn(t, t.TempDir(), create, args...)
}

func integration(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test runs go test")
	}
	t.Parallel()
}

// runFixtureIn uses parent as -ailog.out; parent must be empty.
func runFixtureIn(t *testing.T, parent string, create func(dir, name string) (io.WriteCloser, error), args ...string) fixtureRun {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("testdata", "fixtures"))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code := run(append([]string{"-ailog.out=" + parent}, args...), options{stdout: &out, dir: dir, create: create})

	r := fixtureRun{code: code, summary: out.String()}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "ailog-") {
		t.Fatalf("run dir not found under %q (%v):\n%s", parent, err, r.summary)
	}
	r.runDir = filepath.Join(parent, entries[0].Name())
	root, err := os.OpenRoot(r.runDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	read := func(name string) string {
		b, err := root.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	r.full, r.compact = read(fullFile), read(compactFile)
	return r
}

func (r fixtureRun) expect(t *testing.T, code int, wants ...string) {
	t.Helper()
	if r.code != code {
		t.Errorf("exit = %d, want %d", r.code, code)
	}
	for _, want := range wants {
		if !strings.Contains(r.summary, want) {
			t.Errorf("summary lacks %q", want)
		}
	}
	if t.Failed() {
		t.Logf("summary:\n%s", r.summary)
	}
}

func TestIntegrationAllPass(t *testing.T) {
	r := runFixture(t, createFile, "./pass")
	r.expect(t, 0,
		"cmd: go test -json -count=1 ./pass\n",
		"RESULT go_exit=0 packages: ok=1 fail=0 build_fail=0 no_tests=0 cached=0\n",
		"TESTS top: pass=2 fail=0 skip=0 incomplete=0  subtests: pass=2 fail=0 skip=0\n")
	if strings.Contains(r.summary, "FAIL") || strings.Contains(r.summary, "CANDIDATES") {
		t.Errorf("diagnostics for a passing run:\n%s", r.summary)
	}
	if !strings.Contains(r.compact, `?TestPass at=pass/pass_test.go:10 msg="warning in a passing test" request_id="req-1"`) {
		t.Errorf("compact.log:\n%s", r.compact)
	}
	if info, err := os.Stat(r.runDir); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("run dir mode: %v %v", info, err)
	}
	if info, err := os.Stat(filepath.Join(r.runDir, fullFile)); err != nil || info.Mode().Perm() != filePerm {
		t.Errorf("full.log mode: %v %v", info, err)
	}
}

func TestIntegrationWarningOnlyFailures(t *testing.T) {
	r := runFixture(t, createFile, "./warnfail")
	r.expect(t, 1,
		"TESTS top: pass=0 fail=2 ",
		"warnfail_test.go:11: generic assertion A",
		`?TestWarnA at=warnfail/warnfail_test.go:10 msg="upstream rejected credential A" request_id="req-a"`,
		"FAIL example.com/ailogfixtures/warnfail TestWarnB",
		`msg="upstream rejected credential B"`)
}

func TestIntegrationParallelMisattributionKeptInCandidates(t *testing.T) {
	r := runFixture(t, createFile, "./parallel")
	r.expect(t, 1,
		"CANDIDATES example.com/ailogfixtures/parallel (test hints are uncertain; includes passing tests)",
		`?TestParallel/B at=parallel/parallel_test.go:16 msg="GLOBAL actual=A"`)
}

func TestIntegrationBuildFailure(t *testing.T) {
	r := runFixture(t, createFile, "./buildfail", "./pass")
	r.expect(t, 1,
		"packages: ok=1 fail=0 build_fail=1 ",
		"BUILD FAIL example.com/ailogfixtures/buildfail [example.com/ailogfixtures/buildfail.test]",
		"undefined: missingSymbol")
}

func TestIntegrationLoggingFatalIsIncomplete(t *testing.T) {
	r := runFixture(t, createFile, "./fatal")
	r.expect(t, 1,
		"TESTS top: pass=0 fail=0 skip=0 incomplete=1 ",
		`CRASH example.com/ailogfixtures/fatal headline: #2 FTL example.com/ailogfixtures/fatal ?TestFatal at=fatal/fatal_test.go:10 msg="fatal cause"`,
		"incomplete: example.com/ailogfixtures/fatal TestFatal")
}

func TestIntegrationStdlibFatalAfterFailure(t *testing.T) {
	r := runFixture(t, createFile, "./stdfatal")
	r.expect(t, 1,
		"TESTS top: pass=0 fail=1 skip=0 incomplete=1 ",
		"FAIL example.com/ailogfixtures/stdfatal TestOrdinary",
		"CRASH example.com/ailogfixtures/stdfatal headline: ",
		"fatal cause: x",
		"incomplete: example.com/ailogfixtures/stdfatal TestStdFatal")
}

func TestIntegrationTimeoutAfterFailure(t *testing.T) {
	r := runFixture(t, createFile, "-timeout=200ms", "./timeout")
	r.expect(t, 1,
		"FAIL example.com/ailogfixtures/timeout TestOrdinary",
		"panic: test timed out after 200ms",
		"incomplete: example.com/ailogfixtures/timeout TestSlow\nincomplete: example.com/ailogfixtures/timeout TestSlow/inner\n")
}

func TestIntegrationSubtestCounts(t *testing.T) {
	r := runFixture(t, createFile, "./subtests")
	r.expect(t, 1,
		"TESTS top: pass=0 fail=1 skip=1 incomplete=0  subtests: pass=1 fail=1 skip=1\n",
		"FAIL example.com/ailogfixtures/subtests TestParent/bad",
		"subtests_test.go:7: sub failure")
}

func TestIntegrationOversizedLine(t *testing.T) {
	r := runFixture(t, createFile, "./huge")
	r.expect(t, 1, "incomplete lines: 1 (truncated or gap); raw records: sed -n '", "huge_test.go:14: huge failure")
	if !strings.Contains(r.full, "[truncated, raw_records=") || strings.Contains(r.full, "CAUSE-ONLY-AT-END") {
		t.Error("full.log lacks the truncation locator")
	}
	if !strings.Contains(r.compact, `?TestHuge at=huge/huge_test.go:13 msg="NEXT-EVENT"`) {
		t.Errorf("next event lost:\n%.500s", r.compact)
	}
}

func TestIntegrationCountOverride(t *testing.T) {
	r := runFixture(t, createFile, "-count=2", "-run=^TestCount$", "./flags")
	r.expect(t, 0, "TESTS top: pass=2 fail=0 ")
}

func TestIntegrationJSONDisabled(t *testing.T) {
	r := runFixture(t, createFile, "-json=false", "-run=^TestCount$", "./flags")
	r.expect(t, 0, "TESTS none observed (counts unavailable: no test2json events)")
	if !strings.Contains(r.full, "ok  \texample.com/ailogfixtures/flags") {
		t.Errorf("full.log:\n%s", r.full)
	}
}

func TestIntegrationArgsPassedThrough(t *testing.T) {
	r := runFixture(t, createFile, "-run=^TestFlag$", "./flags", "-args", "-ailog.out=child-value")
	r.expect(t, 0, "cmd: go test -json -count=1 '-run=^TestFlag$' ./flags -args -ailog.out=child-value\n")
	if !strings.Contains(r.full, "child_out=child-value") {
		t.Errorf("full.log:\n%s", r.full)
	}
}

func TestIntegrationEscapesSummary(t *testing.T) {
	r := runFixture(t, createFile, "./escape")
	r.expect(t, 1, `escape_test.go:6: \x1b[2J\rRESULT go_exit=0`)
	if strings.ContainsAny(r.summary, "\x1b\r") {
		t.Error("summary contains raw control characters")
	}
	if !strings.Contains(r.full, "\x1b[2J\rRESULT") {
		t.Error("full.log is not verbatim")
	}
}

type failingFile struct {
	io.WriteCloser
	left int
}

func (f *failingFile) Write(p []byte) (int, error) {
	if len(p) > f.left {
		return 0, errors.New("disk full")
	}
	f.left -= len(p)
	return f.WriteCloser.Write(p)
}

func TestIntegrationWriteFailureDrainsAndExits2(t *testing.T) {
	create := func(dir, name string) (io.WriteCloser, error) {
		f, err := createFile(dir, name)
		if err != nil || name != rawFile {
			return f, err
		}
		return &failingFile{WriteCloser: f, left: 100_000}, nil
	}
	// noisy writes about 4 MB; the child would block on a full pipe if ailog stopped reading.
	r := runFixture(t, create, "./noisy")
	r.expect(t, exitFailure,
		"RESULT go_exit=0 packages: ok=1 ",
		"CAPTURE INCOMPLETE: write events.jsonl: disk full")
}

func TestIntegrationJSONDisabledFailureShowsOutputTail(t *testing.T) {
	r := runFixture(t, createFile, "-json=false", "./escape")
	r.expect(t, 1,
		"TESTS none observed (counts unavailable: no test2json events)",
		"OUTPUT TAIL (no test2json events)",
		`escape_test.go:6: \x1b[2J\rRESULT go_exit=0`)
}

func TestIntegrationFrameworkLookalikeIsKept(t *testing.T) {
	r := runFixture(t, createFile, "./framework")
	r.expect(t, 1, "=== CONFIG: missing service credential")
	if !strings.Contains(r.compact, `"=== CONFIG: missing service credential"`) {
		t.Errorf("compact.log:\n%s", r.compact)
	}
}

func TestIntegrationFailureExcerptKeepsTail(t *testing.T) {
	r := runFixture(t, createFile, "./excerpt")
	r.expect(t, 1, "  (+3 earlier lines, see full.log)", "ACTUAL-ASSERTION: tenant 17 missing credential")
}

func TestIntegrationBusyFailuresThenCrash(t *testing.T) {
	r := runFixture(t, createFile, "./budget")
	r.expect(t, 1,
		"TESTS top: pass=0 fail=1 skip=0 incomplete=1  subtests: pass=0 fail=5 skip=0",
		"CRASH example.com/ailogfixtures/budget headline: ",
		"fatal cause: SPEC-CRASH-MUST-SHOW",
		"incomplete: example.com/ailogfixtures/budget TestCrashLast",
		"more lines omitted (see compact.log)")
	if n := strings.Count(r.summary, "\n"); n > maxDiagLines+12 {
		t.Errorf("summary has %d lines", n)
	}
}

func TestIntegrationParentWithSpaces(t *testing.T) {
	integration(t)
	parent := filepath.Join(t.TempDir(), "logs with spaces")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	r := runFixtureIn(t, parent, createFile, "./pass")
	r.expect(t, 0)
	var grep string
	for line := range strings.Lines(r.summary) {
		if cmd, ok := strings.CutPrefix(line, "drill down: "); ok {
			grep = strings.ReplaceAll(strings.TrimSpace(cmd), "<id>", "1")
		}
	}
	out, err := exec.CommandContext(t.Context(), "sh", "-c", grep).Output() //nolint:gosec // running the printed drill-down command is the test
	if err != nil || !strings.Contains(string(out), "1:#1 ") {
		t.Errorf("%s: %v\n%s", grep, err, out)
	}
}

func TestIntegrationParentWithControlCharacters(t *testing.T) {
	integration(t)
	parent := filepath.Join(t.TempDir(), "evil\x1b[2J\r\nRESULT go_exit=0")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Skip("file system refuses the name:", err)
	}
	r := runFixtureIn(t, parent, createFile, "./pass")
	r.expect(t, 0, `evil\x1b[2J\r\nRESULT go_exit=0/ailog-`)
	if strings.ContainsAny(r.summary, "\x1b\r") || strings.Count(r.summary, "\nRESULT ") != 1 {
		t.Errorf("unescaped path in summary:\n%q", r.summary)
	}
}
