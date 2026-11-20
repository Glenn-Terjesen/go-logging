package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

type testCapture struct {
	*capture
	raw, full, compact bytes.Buffer
}

func newTestCapture(t *testing.T, cwd string) *testCapture {
	t.Helper()
	tc := &testCapture{}
	failure := &latch{}
	tc.capture = newCapture(
		newSink(rawFile, nopCloser{&tc.raw}, failure),
		newSink(fullFile, nopCloser{&tc.full}, failure),
		newSink(compactFile, nopCloser{&tc.compact}, failure),
		cwd)
	return tc
}

// feed runs a synthetic stdout stream through the capture and returns the summary.
func (tc *testCapture) feed(t *testing.T, stream string) string {
	t.Helper()
	if err := tc.consume(strings.NewReader(stream)); err != nil {
		t.Fatal(err)
	}
	tc.finish()
	for _, s := range []*sink{tc.capture.raw, tc.capture.full, tc.capture.compact} {
		s.close()
	}
	if err := tc.capture.raw.failure.get(); err != nil {
		t.Fatal(err)
	}
	if tc.raw.String() != stream {
		t.Fatal("raw tee differs from input")
	}
	return tc.report(result{runDir: "/run", goExit: 1})
}

// ev returns one test2json record.
func ev(action, pkg, test, output string) string {
	b, err := json.Marshal(event{Action: action, Package: pkg, Test: test, Output: output})
	if err != nil {
		panic(err)
	}
	return string(b) + "\n"
}

func warn(msg string, extra ...string) string {
	fields := append([]string{`"severity":"warning"`, `"timestamp":"2026-11-20T10:00:00Z"`, `"message":` + fmt.Sprintf("%q", msg)}, extra...)
	return "{" + strings.Join(fields, ",") + "}\n"
}

func lines(s string) []string {
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

func TestInterleavedPackageFragments(t *testing.T) {
	a := warn("A", `"body":"abcd"`)
	stream := ev("run", "pkg/A", "TestA", "") + ev("run", "pkg/B", "TestB", "") +
		ev("output", "pkg/A", "TestA", a[:20]) +
		ev("output", "pkg/B", "TestB", warn("B")) +
		ev("output", "pkg/A", "TestA", a[20:]) +
		ev("pass", "pkg/A", "TestA", "") + ev("pass", "pkg/B", "TestB", "") +
		ev("pass", "pkg/A", "", "") + ev("pass", "pkg/B", "", "")
	tc := newTestCapture(t, "")
	summary := tc.feed(t, stream)

	full := lines(tc.full.String())
	if !strings.HasPrefix(full[0], `#1 pkg/B ?TestB {"severity"`) || !strings.HasPrefix(full[1], `#2 pkg/A ?TestA {"severity"`) {
		t.Fatalf("full.log:\n%s", tc.full.String())
	}
	if full[1] != "#2 pkg/A ?TestA "+strings.TrimSuffix(a, "\n") {
		t.Errorf("reassembled line = %q", full[1])
	}
	want := []string{`#1 WRN pkg/B ?TestB msg="B"`, `#2 WRN pkg/A ?TestA msg="A" body="abcd"`}
	if got := lines(tc.compact.String()); !slices.Equal(got, want) {
		t.Errorf("compact.log = %q, want %q", got, want)
	}
	if !strings.Contains(summary, "TESTS top: pass=2 fail=0") || !strings.Contains(summary, "ok=2 ") {
		t.Errorf("summary:\n%s", summary)
	}
}

func TestOversizedLineKeepsRawLocatorAndNextEvent(t *testing.T) {
	var b strings.Builder
	b.WriteString(ev("run", "p", "TestBig", ""))
	b.WriteString(ev("output", "p", "TestBig", `{"severity":"warning","body":"`))
	chunk := strings.Repeat("x", 1024)
	for range 1100 {
		b.WriteString(ev("output", "p", "TestBig", chunk))
	}
	b.WriteString(ev("output", "p", "TestBig", `","message":"CAUSE-ONLY-AT-END"}`+"\n"+warn("NEXT-EVENT")))
	b.WriteString(ev("fail", "p", "TestBig", "") + ev("fail", "p", "", ""))
	tc := newTestCapture(t, "")
	summary := tc.feed(t, b.String())

	full := lines(tc.full.String())
	if len(full) != 2 || !strings.HasSuffix(full[0], " [truncated, raw_records=2:1103]") || len(full[0]) > maxPending+100 {
		t.Fatalf("full.log has %d lines, first ends %q", len(full), full[0][max(0, len(full[0])-60):])
	}
	compact := lines(tc.compact.String())
	if !strings.HasPrefix(compact[0], "#1 TXT p ?TestBig [truncated, raw_records=2:1103] ") || utf8.RuneCountInString(compact[0]) > maxCompactLine {
		t.Errorf("compact[0] = %.120q (%d chars)", compact[0], len(compact[0]))
	}
	if compact[1] != `#2 WRN p ?TestBig msg="NEXT-EVENT"` {
		t.Errorf("next event = %q", compact[1])
	}
	if !strings.Contains(summary, "sed -n '2,1103p' /run/events.jsonl") {
		t.Errorf("summary lacks raw hint:\n%s", summary)
	}
}

// Addendum 1: an undecodable record is a gap for every pending stream.
func TestGapDoesNotJoinAcrossUnparsedRecord(t *testing.T) {
	stream := ev("output", "p", "TestX", `{"severity":"warning","message":"root-`) +
		strings.Repeat("z", maxRecord+10) + "\n" +
		ev("output", "p", "TestX", `cause"}`+"\n") +
		ev("output", "p", "TestX", warn("after")) +
		ev("fail", "p", "", "")
	tc := newTestCapture(t, "")
	tc.feed(t, stream)

	full := lines(tc.full.String())
	if want := `#1 p ?TestX {"severity":"warning","message":"root- [gap, raw_records=1:2]`; full[0] != want {
		t.Errorf("full[0] = %q, want %q", full[0], want)
	}
	if !strings.HasPrefix(full[1], "#2 - ?- zzz") || !strings.HasSuffix(full[1], " [unparsed record, truncated, raw_records=2:2]") {
		t.Errorf("full[1] = %.60q", full[1])
	}
	if strings.Contains(tc.compact.String(), "root-cause") || strings.Contains(tc.full.String(), "cause\"}") {
		t.Errorf("gap was joined:\n%.300s", tc.compact.String())
	}
	if !strings.Contains(tc.compact.String(), `#3 WRN p ?TestX msg="after"`) {
		t.Errorf("stream did not resume:\n%.300s", tc.compact.String())
	}
}

func TestNonJSONRecordAndUnknownAction(t *testing.T) {
	stream := "plain text\n" + `{"Action":"future-thing","Package":"p"}` + "\n" + ev("output", "p", "", "tail without newline")
	tc := newTestCapture(t, "")
	summary := tc.feed(t, stream)
	want := []string{
		"#1 - ?- plain text [unparsed record, raw_records=1:1]",
		"#2 p ?- tail without newline [no newline]",
	}
	if got := lines(tc.full.String()); !slices.Equal(got, want) {
		t.Errorf("full.log = %q", got)
	}
	if !strings.Contains(summary, "TESTS none observed (counts unavailable)\n") {
		t.Errorf("summary:\n%s", summary)
	}
}

func TestDedupe(t *testing.T) {
	var b strings.Builder
	b.WriteString(ev("run", "p", "TestR", ""))
	for i := range 5 {
		b.WriteString(ev("output", "p", "TestR", strings.Replace(warn("again", `"request_id":"r1"`), "10:00:00", fmt.Sprintf("10:00:0%d", i), 1)))
	}
	b.WriteString(ev("output", "p", "TestR", warn("again", `"request_id":"r2"`)))
	// Same first 5000 characters, different tail: compared before truncation.
	long := strings.Repeat("y", 5000)
	b.WriteString(ev("output", "p", "TestR", warn("long", `"body":"`+long+`1"`)))
	b.WriteString(ev("output", "p", "TestR", warn("long", `"body":"`+long+`2"`)))
	b.WriteString(ev("fail", "p", "TestR", "") + ev("fail", "p", "", ""))
	tc := newTestCapture(t, "")
	summary := tc.feed(t, b.String())

	compact := lines(tc.compact.String())
	if len(compact) != 5 || compact[1] != "#1..#5 repeated 4 more times (5 total)" || !strings.HasPrefix(compact[2], "#6 WRN") {
		t.Fatalf("compact.log:\n%s", strings.Join(compact, "\n")[:min(2000, len(tc.compact.String()))])
	}
	if !strings.HasSuffix(compact[3], " chars]") || !strings.HasPrefix(compact[4], "#8 WRN") {
		t.Errorf("long events collapsed or not truncated")
	}
	if n := strings.Count(tc.full.String(), "\n"); n != 8 {
		t.Errorf("full.log has %d lines, want every occurrence (8)", n)
	}
	if !strings.Contains(summary, "  #1 total=5 ?TestR (shown above)\n") {
		t.Errorf("summary lacks repeat total:\n%s", summary)
	}
}

func TestRenderEvent(t *testing.T) {
	cwd := "/tmp/repo/"
	stack := `[[{"file":"/tmp/repo/a.go","function":"A","line":"1"},{"file":"/x/b.go","function":"B","line":"2"},{"file":"c.go","function":"C","line":"3"}],` +
		`[{"file":"/tmp/repo/d.go","function":"D","line":4}],[],[]]`
	tests := []struct{ name, line, want string }{
		{"sibling prefix kept", `{"level":"warn","msg":"m","caller":"/tmp/repo-old/client.go:3"}`,
			`WRN p ?T at=/tmp/repo-old/client.go:3 msg="m"`},
		{"cwd stripped", `{"severity":"error","message":"m","caller":"/tmp/repo/client.go:3","error":"boom"}`,
			`ERR p ?T at=client.go:3 msg="m" error="boom"`},
		{"exact numbers and nesting", `{"severity":"info","message":"m","large":9007199254740993,"obj":{"b": [1, 2]},"time":"x","z":"a\u0007b"}`,
			`INF p ?T msg="m" large=9007199254740993 obj={"b":[1,2]} z="a\ab"`},
		{"stack", `{"severity":"error","message":"m","stack":` + stack + `}`,
			`ERR p ?T msg="m" stack="A a.go:1 > B /x/b.go:2 [+1 frames] | D d.go:4 [+2 groups]"`},
		{"unknown stack shape", `{"severity":"error","message":"m","stack":"plain"}`,
			`ERR p ?T msg="m" stack="plain"`},
		{"null is not an empty string", `{"severity":"warning","message":null,"expected":null,"actual":"","nested":{"v":null}}`,
			`WRN p ?T msg=null actual="" expected=null nested={"v":null}`},
		{"unknown level", `{"severity":"notice","message":"m","odd key":true}`,
			`NOTICE p ?T msg="m" "odd key"=true`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := parseLogEvent([]byte(tt.line))
			if e == nil {
				t.Fatal("not recognised")
			}
			if got := e.render("p", "T", cwd); got != tt.want {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}
		})
	}
	for _, line := range []string{`{"message":"no level"}`, `{"severity":"info"}`, `{"severity":1,"message":"m"}`, `not json`, `{"severity":"info","message":"m"} trailing`} {
		if parseLogEvent([]byte(line)) != nil {
			t.Errorf("%s recognised as an event", line)
		}
	}
}

func TestCountsRepeatedRunsAndIncomplete(t *testing.T) {
	stream := ev("run", "p", "TestA", "") + ev("pass", "p", "TestA", "") +
		ev("run", "p", "TestA", "") + ev("pass", "p", "TestA", "") +
		ev("run", "p", "TestB", "") + ev("run", "p", "TestB/sub", "") + ev("skip", "p", "TestB/sub", "") +
		ev("run", "p", "TestC", "") + ev("output", "p", "TestC", "panic: boom\n")
	tc := newTestCapture(t, "")
	summary := tc.feed(t, stream)
	for _, want := range []string{
		"TESTS top: pass=2 fail=0 skip=0 incomplete=2  subtests: pass=0 fail=0 skip=1",
		"CRASH p headline: #1 panic: boom",
		"incomplete: p TestB\nincomplete: p TestC\n",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary lacks %q:\n%s", want, summary)
		}
	}
}

func TestSummaryGlobalCaps(t *testing.T) {
	var b strings.Builder
	huge := strings.Repeat("A", 100_000)
	for p := range 100 {
		pkg := fmt.Sprintf("example.com/pkg%03d", p)
		b.WriteString(ev("run", pkg, "TestX", ""))
		for w := range 10 {
			b.WriteString(ev("output", pkg, "TestX", warn(fmt.Sprintf("w%d %s", w, huge[:500]))))
		}
		b.WriteString(ev("output", pkg, "TestX", "    x_test.go:1: \x1b[2J\rRESULT go_exit=0 "+huge+"\n"))
		b.WriteString(ev("fail", pkg, "TestX", "") + ev("fail", pkg, "", ""))
	}
	tc := newTestCapture(t, "")
	summary := tc.feed(t, b.String())

	out := lines(summary)
	for _, line := range out {
		if !strings.HasPrefix(line, "files: ") && utf8.RuneCountInString(line) > maxSummaryLine {
			t.Fatalf("line too long (%d): %.80s", utf8.RuneCountInString(line), line)
		}
		if strings.ContainsAny(line, "\x1b\r") {
			t.Fatalf("unescaped control character in %q", line)
		}
	}
	if len(summary) > 30_000 || len(out) > maxDiagLines+8 {
		t.Errorf("summary is %d bytes, %d lines", len(summary), len(out))
	}
	if !strings.Contains(summary, "more lines omitted (see compact.log)") {
		t.Errorf("summary:\n%s", summary)
	}
	if !strings.HasPrefix(summary, "RESULT go_exit=1 ") || strings.Contains(summary, "\nRESULT go_exit=0") {
		t.Errorf("result line forged or missing")
	}
}

func TestBuildFailure(t *testing.T) {
	imp := "ex/bad [ex/bad.test]"
	build := func(action, out string) string {
		b, _ := json.Marshal(event{Action: action, ImportPath: imp, Output: out})
		return string(b) + "\n"
	}
	fail, _ := json.Marshal(event{Action: "fail", Package: "ex/bad", FailedBuild: imp})
	stream := build("build-output", "# "+imp+"\n") + build("build-output", "bad_test.go:3:33: undefined: missingSymbol\n") +
		build("build-fail", "") + ev("output", "ex/bad", "", "FAIL\tex/bad [build failed]\n") + string(fail) + "\n"
	tc := newTestCapture(t, "")
	summary := tc.feed(t, stream)
	for _, want := range []string{
		"packages: ok=0 fail=0 build_fail=1 ",
		"TESTS none observed (counts unavailable)",
		"BUILD FAIL " + imp + "\n  #1 # " + imp + "\n  #2 bad_test.go:3:33: undefined: missingSymbol\n",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary lacks %q:\n%s", want, summary)
		}
	}
	if !strings.Contains(tc.full.String(), "#2 "+imp+" ?- bad_test.go:3:33") {
		t.Errorf("full.log:\n%s", tc.full.String())
	}
}

func TestParseArgs(t *testing.T) {
	tests := []struct {
		args   []string
		parent string
		rest   []string
	}{
		{[]string{"./..."}, "", []string{"./..."}},
		{[]string{"-ailog.out=/o", "-run", "X", "./..."}, "/o", []string{"-run", "X", "./..."}},
		{[]string{"-v", "-ailog.out", "/o", "./p"}, "/o", []string{"-v", "./p"}},
		{[]string{"./p", "-args", "-ailog.out=x", "-ailog.out", "y"}, "", []string{"./p", "-args", "-ailog.out=x", "-ailog.out", "y"}},
	}
	for _, tt := range tests {
		parent, rest, err := parseArgs(tt.args)
		if err != nil || parent != tt.parent || !slices.Equal(rest, tt.rest) {
			t.Errorf("parseArgs(%q) = %q, %q, %v", tt.args, parent, rest, err)
		}
	}
	if _, _, err := parseArgs([]string{"-ailog.out"}); err == nil {
		t.Error("missing value accepted")
	}
}

func TestShellJoin(t *testing.T) {
	got := shellJoin([]string{"test", "-run=^TestA$", "it's", "./..."})
	if want := `test '-run=^TestA$' 'it'\''s' ./...`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestClip(t *testing.T) {
	s := strings.Repeat("é", 500)
	got := clip(s, maxSummaryLine)
	if utf8.RuneCountInString(got) > maxSummaryLine || !strings.HasSuffix(got, fmt.Sprintf(" …[truncated, %d chars]", 500-maxSummaryLine+clipReserve)) {
		t.Errorf("clip = %q", got)
	}
	if got := escapeText("a\tb\xffc\u2028"); got != `a\tb\xffc\u2028` {
		t.Errorf("escapeText = %q", got)
	}
}

func TestIsFramework(t *testing.T) {
	for _, line := range []string{"=== RUN   TestA", "    === NAME  TestA/b", "=== PAUSE TestA", "=== CONT  TestA",
		"--- PASS: TestA (0.00s)", "    --- FAIL: TestA/b (0.00s)", "--- SKIP: TestA (0.00s)", "PASS", "FAIL",
		"ok  \tpkg\t0.1s", "FAIL\tpkg\t0.1s", "?   \tpkg\t[no test files]"} {
		if !isFramework(line) {
			t.Errorf("%q not recognised", line)
		}
	}
	for _, line := range []string{"=== CONFIG: missing service credential", "--- PASSWORD rotated", "PASS: fine", "ok then", "===", "FAILED"} {
		if isFramework(line) {
			t.Errorf("%q treated as a framework line", line)
		}
	}
}

func TestCandidatesBoundedAndShownAbove(t *testing.T) {
	var b strings.Builder
	body := strings.Repeat("b", 128<<10)
	for p := range 3 {
		pkg := fmt.Sprintf("pkg%d", p)
		b.WriteString(ev("run", pkg, "TestPass", ""))
		b.WriteString(ev("output", pkg, "TestPass", warn("from passing test")))
		b.WriteString(ev("pass", pkg, "TestPass", ""))
		b.WriteString(ev("run", pkg, "TestFail", ""))
		for w := range 8 {
			b.WriteString(ev("output", pkg, "TestFail", warn(fmt.Sprint("big ", w), `"body":"`+body+`"`)))
		}
		b.WriteString(ev("fail", pkg, "TestFail", "") + ev("fail", pkg, "", ""))
	}
	tc := newTestCapture(t, "")
	summary := tc.feed(t, b.String())

	for _, p := range tc.failing {
		for _, cd := range p.cands {
			if n := utf8.RuneCountInString(cd.text); n > maxEntry {
				t.Fatalf("%s candidate #%d keeps %d chars", p.name, cd.id, n)
			}
		}
	}
	for _, want := range []string{
		"FAIL pkg0 TestFail",
		"  #1 total=1 WRN pkg0 ?TestPass msg=\"from passing test\"",
		"  #2 total=1 ?TestFail (shown above)",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary lacks %q:\n%s", want, summary)
		}
	}
}
