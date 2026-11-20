package main

import (
	"cmp"
	"fmt"
	"path/filepath"
	"strings"
)

const maxDiagLines = 60

// Diagnostic line priorities: the 60-line budget goes to core lines first, then
// headers, then details; lines are still printed in section order.
const (
	tierCore   = iota // BUILD FAIL and FAIL headers, crash headlines, incomplete tests
	tierHeader        // CANDIDATES headers with their first candidate
	tierDetail        // everything else
)

type result struct {
	runDir     string
	goExit     int
	captureErr error
	stderr     *headLines
}

type diagLine struct {
	text  string
	tier  int
	shows int    // #id of the event this FAIL block line prints
	ref   int    // #id a candidate line repeats
	short string // candidate line used when ref was printed in a FAIL block
}

type diag struct {
	items []diagLine
}

// add escapes and clips one diagnostic line; the result may be amended before the next add.
func (d *diag) add(tier int, format string, args ...any) *diagLine {
	d.items = append(d.items, diagLine{text: clip(escapeText(fmt.Sprintf(format, args...)), maxSummaryLine), tier: tier})
	return &d.items[len(d.items)-1]
}

// lines allocates the budget by tier and returns the kept lines in their original order.
func (d *diag) lines() []string {
	keep := make([]bool, len(d.items))
	budget := maxDiagLines
	for tier := tierCore; tier <= tierDetail; tier++ {
		for i, item := range d.items {
			if item.tier == tier && budget > 0 {
				keep[i] = true
				budget--
			}
		}
	}
	printed := map[int]bool{}
	for i, item := range d.items {
		if keep[i] && item.shows > 0 {
			printed[item.shows] = true
		}
	}

	var out []string
	omitted := 0
	for i, item := range d.items {
		switch {
		case !keep[i]:
			omitted++
		case item.ref > 0 && printed[item.ref]:
			out = append(out, item.short)
		default:
			out = append(out, item.text)
		}
	}
	if omitted > 0 {
		out = append(out, fmt.Sprintf("... %d more lines omitted (see compact.log)", omitted))
	}
	return out
}

// report renders the summary printed after the child has exited.
func (c *capture) report(res result) string {
	path := func(name string) string { return filepath.Join(res.runDir, name) }
	out := []string{fmt.Sprintf("RESULT go_exit=%d packages: ok=%d fail=%d build_fail=%d no_tests=%d cached=%d",
		res.goExit, c.ok, c.fail, c.buildFail, c.noTests, c.hit)}
	if res.captureErr != nil {
		out = append(out, clip(escapeText("CAPTURE INCOMPLETE: "+res.captureErr.Error()), maxSummaryLine))
	}
	switch {
	case !c.sawEvent:
		out = append(out, "TESTS none observed (counts unavailable: no test2json events)")
	case !c.sawNamed:
		out = append(out, "TESTS none observed (counts unavailable)")
	default:
		out = append(out, fmt.Sprintf("TESTS top: pass=%d fail=%d skip=%d incomplete=%d  subtests: pass=%d fail=%d skip=%d",
			c.top.pass, c.top.fail, c.top.skip, c.top.incomplete, c.sub.pass, c.sub.fail, c.sub.skip))
	}

	d := &diag{}
	c.diagnostics(d, res)
	out = append(out, d.lines()...)

	// Metadata lines are escaped but never clipped, so paths stay usable.
	for _, f := range []struct{ label, name string }{{"full", fullFile}, {"compact", compactFile}, {"raw", rawFile}, {"stderr", stderrFile}} {
		out = append(out, f.label+": "+escapeText(path(f.name)))
	}
	out = append(out, "drill down: grep -n '^#<id> ' "+escapeText(shellQuote(path(fullFile))))
	if c.lossy > 0 {
		out = append(out, fmt.Sprintf("incomplete lines: %d (truncated or gap); raw records: sed -n '%sp' %s",
			c.lossy, c.firstLossy, escapeText(shellQuote(path(rawFile)))))
	}
	return strings.Join(out, "\n") + "\n"
}

func (c *capture) diagnostics(d *diag, res result) {
	if h := res.stderr; h != nil && h.total > 0 {
		d.add(tierDetail, "STDERR %d lines", h.total)
		for _, line := range h.lines {
			d.add(tierDetail, "  %s", line)
		}
	}
	if p := c.pkgs[textPkg]; !c.sawEvent && res.goExit != 0 && p != nil {
		d.add(tierDetail, "OUTPUT TAIL (no test2json events)")
		for _, line := range p.tail {
			d.add(tierDetail, "  %s", line)
		}
	}

	for _, path := range c.builds {
		d.add(tierCore, "BUILD FAIL %s", path)
		for _, line := range c.buildLines[path] {
			d.add(tierDetail, "  %s", line)
		}
	}

	for _, b := range c.failBlocks {
		d.add(tierCore, "FAIL %s %s (#%d..#%d)", b.pkg, b.name, b.first, b.last)
		if b.dropped > 0 {
			d.add(tierDetail, "  (+%d earlier lines, see full.log)", b.dropped)
		}
		for _, line := range b.lines {
			d.add(tierDetail, "  %s", line)
		}
		for _, ev := range b.events {
			d.add(tierDetail, "  %s", ev.text).shows = ev.id
		}
	}
	if c.moreFailed > 0 {
		d.add(tierCore, "... %d more failed tests not shown (grep -e '--- FAIL' full.log)", c.moreFailed)
	}

	for _, p := range c.failing {
		d.add(tierHeader, "CANDIDATES %s (test hints are uncertain; includes passing tests)", p.name)
		for i, cd := range p.cands {
			tier := tierDetail
			if i == 0 {
				tier = tierHeader
			}
			line := d.add(tier, "  #%d total=%d %s", cd.id, cd.total, cd.text)
			line.ref = cd.id
			line.short = clip(escapeText(fmt.Sprintf("  #%d total=%d ?%s (shown above)", cd.id, cd.total, cd.hint)), maxSummaryLine)
		}
		if len(p.cands) == 0 {
			d.add(tierHeader, "  (no WRN/ERR/FTL/PNC events)")
		}
		if p.otherWarns > 0 {
			d.add(tierDetail, "  ... %d more WRN/ERR/FTL/PNC events in compact.log", p.otherWarns)
		}
		if p.crashed || len(p.incomplete) > 0 {
			d.add(tierCore, "CRASH %s headline: %s", p.name, cmp.Or(p.headline, p.lastLine, "(none)"))
			for _, line := range p.tail {
				d.add(tierDetail, "  %s", line)
			}
		}
	}

	for _, p := range c.failing {
		for _, name := range p.incomplete {
			d.add(tierCore, "incomplete: %s %s", p.name, name)
		}
	}
}
