package main

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
)

const (
	maxRecord     = 1 << 20 // one stdout record (test2json event)
	maxPending    = 1 << 20 // one reassembled logical line
	readBufSize   = 64 << 10
	maxBlockLines = 8
	maxCandidates = 8
	maxFailBlocks = 5
	maxTailLines  = 20
	maxBuildLines = 30
)

const (
	buildKey = "build\x00" // stream key prefix for build output, keyed by ImportPath
	textPkg  = "-"         // pseudo package for stdout records that are not test2json
)

// How a pending logical line ended.
const (
	endNewline = iota
	endFlush   // package terminal event or EOF without a newline
	endGap     // an undecodable stdout record interrupted the stream
)

// event is a test2json record (go doc test2json).
type event struct {
	Action      string
	Package     string
	ImportPath  string
	Test        string
	Output      string
	FailedBuild string
}

// stream reassembles Output fragments of one package (or build) into lines.
type stream struct {
	key, pkg string
	test     string // Test of the last event that contributed to the pending line
	buf      []byte
	open     bool // a logical line is pending (possibly empty)
	start    int  // raw record where the pending line began
	cut      bool // pending line exceeded maxPending; the rest is discarded
	skip     bool // discard up to the next newline after a gap
}

// logical is one completed line, as written to full.log.
type logical struct {
	key, pkg, test string
	text           []byte
	mark           string // full.log suffix, e.g. " [no newline]"
	opaque         bool   // incomplete content: never parse as JSON
	build          bool
}

type tally struct{ pass, fail, skip, incomplete int }

type testBlock struct {
	pkg, name   string
	first, last int
	lines       []string // the last maxBlockLines non-log lines
	dropped     int      // earlier non-log lines not kept
	events      []ref
}

// ref is a bounded summary entry for one logical line.
type ref struct {
	id   int
	text string
}

type candidate struct {
	ref
	total int
	hint  string
}

type pkgState struct {
	name       string
	active     map[string]*testBlock
	incomplete []string
	failed     int
	crashed    bool // package failed without a failed test
	cached     bool
	tail       []string
	headline   string
	lastLine   string
	cands      []*candidate
	candIndex  map[[32]byte]*candidate
	otherWarns int
}

type repeat struct {
	key                [32]byte
	hint               string
	first, last, extra int
}

type capture struct {
	raw, full, compact *sink
	cwd                string

	rawLine, seq       int
	sawEvent, sawNamed bool
	streams            map[string]*stream
	repeats            map[string]*repeat
	pkgs               map[string]*pkgState

	top, sub                          tally
	ok, fail, buildFail, noTests, hit int
	failing                           []*pkgState
	failBlocks                        []*testBlock
	moreFailed                        int
	builds                            []string
	buildLines                        map[string][]string
	buildLineCount                    int
	lossy                             int
	firstLossy                        string
}

func newCapture(raw, full, compact *sink, cwd string) *capture {
	return &capture{
		raw: raw, full: full, compact: compact, cwd: cwd,
		streams:    map[string]*stream{},
		repeats:    map[string]*repeat{},
		pkgs:       map[string]*pkgState{},
		buildLines: map[string][]string{},
	}
}

// consume tees r into the raw file and processes it one newline-framed record at a time.
func (c *capture) consume(r io.Reader) error {
	return frameLines(r, c.raw.write, maxRecord, func(rec []byte, oversized bool) {
		c.rawLine++
		c.record(rec, oversized)
	})
}

// frameLines passes every byte of r to tee and each newline-terminated record (or final
// unterminated one), without its newline and bounded to limit bytes, to emit.
func frameLines(r io.Reader, tee func([]byte), limit int, emit func(rec []byte, cut bool)) error {
	br := bufio.NewReaderSize(r, readBufSize)
	rec := make([]byte, 0, min(limit, readBufSize))
	pending, cut := false, false
	for {
		chunk, err := br.ReadSlice('\n')
		tee(chunk)
		pending = pending || len(chunk) > 0
		chunk = bytes.TrimSuffix(chunk, []byte("\n"))
		if room := limit - len(rec); len(chunk) > room {
			cut = true
			chunk = chunk[:room]
		}
		rec = append(rec, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if pending {
			emit(rec, cut)
		}
		rec, pending, cut = rec[:0], false, false
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// record handles one stdout record; oversized records are only kept as a truncated text line.
func (c *capture) record(rec []byte, oversized bool) {
	l := logical{key: textPkg, pkg: textPkg, text: rec}
	var ev event
	switch {
	case oversized:
		l.mark = fmt.Sprintf(" [unparsed record, truncated, raw_records=%d:%d]", c.rawLine, c.rawLine)
		l.opaque = true
		c.noteLossy(c.rawLine, c.rawLine)
	case json.Unmarshal(rec, &ev) == nil && ev.Action != "":
		c.sawEvent = true
		c.handle(&ev)
		return
	default:
		l.mark = fmt.Sprintf(" [unparsed record, raw_records=%d:%d]", c.rawLine, c.rawLine)
	}

	// The stream this record belonged to is unknown, so every pending line has a hole.
	for _, key := range slices.Sorted(maps.Keys(c.streams)) {
		if st := c.streams[key]; st.open {
			c.complete(st, endGap)
			st.skip = true
		}
	}
	c.line(l)
}

func (c *capture) handle(ev *event) {
	switch ev.Action {
	case "build-output":
		c.output(buildKey+ev.ImportPath, ev.ImportPath, "", ev.Output)
	case "build-fail":
		c.flush(buildKey + ev.ImportPath)
		c.addBuild(ev.ImportPath)
	case "output":
		c.output(ev.Package, ev.Package, ev.Test, ev.Output)
	case "run":
		if ev.Test != "" {
			c.sawNamed = true
			c.pkg(ev.Package).active[ev.Test] = &testBlock{pkg: ev.Package, name: ev.Test}
		}
	case "pass", "fail", "skip":
		if ev.Test != "" {
			c.testDone(ev)
		} else if ev.Package != "" {
			c.pkgDone(ev)
		}
	default: // start, pause, cont, bench, attr, ... and future actions: kept in the raw file only
	}
}

func (c *capture) output(key, pkg, test, out string) {
	st := c.streams[key]
	if st == nil {
		st = &stream{key: key, pkg: pkg}
		c.streams[key] = st
	}
	for out != "" {
		piece, rest, newline := strings.Cut(out, "\n")
		out = rest
		if st.skip {
			st.skip = !newline
			continue
		}
		if !st.open {
			st.open, st.start = true, c.rawLine
		}
		st.test = test
		if room := maxPending - len(st.buf); len(piece) > room {
			st.cut = true
			piece = piece[:room]
		}
		st.buf = append(st.buf, piece...)
		if newline {
			c.complete(st, endNewline)
		}
	}
}

func (c *capture) complete(st *stream, end int) {
	var mark string
	switch {
	case end == endGap:
		mark = fmt.Sprintf(" [gap, raw_records=%d:%d]", st.start, c.rawLine)
		c.noteLossy(st.start, c.rawLine)
	case st.cut:
		mark = fmt.Sprintf(" [truncated, raw_records=%d:%d]", st.start, c.rawLine)
		c.noteLossy(st.start, c.rawLine)
	default:
	}
	if end == endFlush {
		mark += " [no newline]"
	}
	c.line(logical{
		key: st.key, pkg: st.pkg, test: st.test, text: st.buf, mark: mark,
		opaque: st.cut || end == endGap, build: strings.HasPrefix(st.key, buildKey),
	})
	st.buf, st.open, st.cut = st.buf[:0], false, false
	if cap(st.buf) > readBufSize {
		st.buf = nil
	}
}

func (c *capture) flush(key string) {
	if st := c.streams[key]; st != nil && st.open {
		c.complete(st, endFlush)
	}
	delete(c.streams, key)
	c.flushRepeat(key)
}

// line numbers a completed logical line and writes it to full.log, compact.log and the summary state.
func (c *capture) line(l logical) {
	c.seq++
	id := c.seq
	hint := cmp.Or(l.test, "-")
	c.full.printf("#%d %s ?%s %s%s\n", id, l.pkg, hint, l.text, l.mark)

	var ev *logEvent
	if !l.opaque {
		ev = parseLogEvent(l.text)
	}
	text := string(l.text)
	framework := ev == nil && isFramework(text)
	var body string
	if ev != nil {
		body = ev.render(l.pkg, hint, c.cwd)
	} else {
		body = fmt.Sprintf("TXT %s ?%s%s %s", l.pkg, hint, l.mark, strconv.Quote(text))
	}

	if r := c.repeats[l.key]; ev != nil && r != nil && r.key == ev.key && r.hint == hint {
		r.last, ev.repeated = id, true
		r.extra++
	} else {
		c.flushRepeat(l.key)
		if !framework {
			c.compact.printf("%s\n", clip(fmt.Sprintf("#%d %s", id, body), maxCompactLine))
		}
		if ev != nil {
			c.repeats[l.key] = &repeat{key: ev.key, hint: hint, first: id, last: id}
		}
	}

	if l.build {
		if c.buildLineCount < maxBuildLines {
			c.buildLineCount++
			c.buildLines[l.pkg] = append(c.buildLines[l.pkg], entry(id, escapeText(text)))
		}
		return
	}
	c.track(c.pkg(l.pkg), l, id, ev, body)
}

// track keeps the bounded per-package and per-test state used by the summary.
func (c *capture) track(p *pkgState, l logical, id int, ev *logEvent, body string) {
	text := string(l.text)
	b := p.active[l.test]
	if b != nil {
		b.first = cmp.Or(b.first, id)
		b.last = id
	}

	if ev == nil {
		if isFramework(text) {
			p.cached = p.cached || strings.HasPrefix(text, "ok  ") && strings.Contains(text, "(cached)")
			return
		}
		e := entry(id, escapeText(strings.TrimPrefix(l.mark+" ", " ")+text))
		if b != nil {
			b.lines = append(b.lines, e)
			if len(b.lines) > maxBlockLines {
				b.lines = b.lines[1:]
				b.dropped++
			}
		}
		p.tail = append(p.tail, e)
		if len(p.tail) > maxTailLines {
			p.tail = p.tail[1:]
		}
		if p.headline == "" && isHeadline(text) {
			p.headline = e
		}
		if !strings.HasPrefix(text, "exit status ") {
			p.lastLine = e
		}
		return
	}

	e := entry(id, body)
	p.lastLine = e
	if !ev.severe() {
		return
	}
	if b != nil && !ev.repeated && len(b.events) < maxBlockLines {
		b.events = append(b.events, ref{id: id, text: e})
	}
	switch cd := p.candIndex[ev.key]; {
	case cd != nil:
		cd.total++
	case len(p.cands) < maxCandidates:
		// Identity is the full event (ev.key); only a bounded display text is kept.
		cd = &candidate{ref: ref{id: id, text: clip(body, maxEntry)}, total: 1, hint: cmp.Or(l.test, "-")}
		p.cands = append(p.cands, cd)
		p.candIndex[ev.key] = cd
	default:
		p.otherWarns++
	}
}

func (c *capture) flushRepeat(key string) {
	if r := c.repeats[key]; r != nil && r.extra > 0 {
		c.compact.printf("#%d..#%d repeated %d more times (%d total)\n", r.first, r.last, r.extra, r.extra+1)
	}
	delete(c.repeats, key)
}

func (c *capture) testDone(ev *event) {
	c.sawNamed = true
	p := c.pkg(ev.Package)
	b := p.active[ev.Test]
	delete(p.active, ev.Test)

	t := &c.top
	if strings.Contains(ev.Test, "/") {
		t = &c.sub
	}
	switch ev.Action {
	case "pass":
		t.pass++
	case "skip":
		t.skip++
	default:
		t.fail++
		p.failed++
		if len(c.failBlocks) == maxFailBlocks {
			c.moreFailed++
			return
		}
		if b == nil {
			b = &testBlock{pkg: ev.Package, name: ev.Test}
		}
		c.failBlocks = append(c.failBlocks, b)
	}
}

func (c *capture) pkgDone(ev *event) {
	c.flush(ev.Package)
	p := c.pkg(ev.Package)
	delete(c.pkgs, ev.Package)
	c.closeTests(p)
	if p.cached {
		c.hit++
	}

	switch {
	case ev.FailedBuild != "":
		c.buildFail++
		c.addBuild(ev.FailedBuild)
		return
	case ev.Action == "pass":
		c.ok++
	case ev.Action == "skip":
		c.noTests++
	default:
		c.fail++
		p.crashed = p.failed == 0
	}
	if p.failed > 0 || p.crashed || len(p.incomplete) > 0 {
		c.failing = append(c.failing, p)
	}
}

// closeTests turns tests that started but never finished into incomplete ones.
func (c *capture) closeTests(p *pkgState) {
	for _, name := range slices.Sorted(maps.Keys(p.active)) {
		p.incomplete = append(p.incomplete, name)
		if !strings.Contains(name, "/") {
			c.top.incomplete++
		}
	}
	clear(p.active)
}

// finish flushes everything still pending at EOF.
func (c *capture) finish() {
	for _, key := range slices.Sorted(maps.Keys(c.streams)) {
		c.flush(key)
	}
	for _, key := range slices.Sorted(maps.Keys(c.repeats)) {
		c.flushRepeat(key)
	}
	for _, name := range slices.Sorted(maps.Keys(c.pkgs)) {
		if p := c.pkgs[name]; len(p.active) > 0 {
			c.closeTests(p)
			c.failing = append(c.failing, p)
		}
	}
}

func (c *capture) pkg(name string) *pkgState {
	p := c.pkgs[name]
	if p == nil {
		p = &pkgState{name: name, active: map[string]*testBlock{}, candIndex: map[[32]byte]*candidate{}}
		c.pkgs[name] = p
	}
	return p
}

func (c *capture) addBuild(path string) {
	if !slices.Contains(c.builds, path) {
		c.builds = append(c.builds, path)
	}
}

func (c *capture) noteLossy(start, end int) {
	c.lossy++
	if c.firstLossy == "" {
		c.firstLossy = fmt.Sprintf("%d,%d", start, end)
	}
}

// isFramework reports lines printed by the testing package or go test itself.
func isFramework(text string) bool {
	trimmed := strings.TrimLeft(text, " ")
	for _, prefix := range []string{"=== RUN ", "=== PAUSE ", "=== CONT ", "=== NAME ", "--- PASS: ", "--- FAIL: ", "--- SKIP: "} {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	for _, prefix := range []string{"ok  \t", "FAIL\t", "?   \t"} {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return text == "PASS" || text == "FAIL"
}

func isHeadline(text string) bool {
	trimmed := strings.TrimSpace(text)
	return strings.HasPrefix(trimmed, "panic:") || strings.HasPrefix(trimmed, "fatal error:") || strings.Contains(trimmed, "test timed out")
}
