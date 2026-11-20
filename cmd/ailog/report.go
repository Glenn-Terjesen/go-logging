package main

import (
	"cmp"
	"fmt"
	"path/filepath"
	"strings"
)

const maxDiagLines = 60

type result struct {
	runDir     string
	goExit     int
	captureErr error
	stderr     *headLines
}

// diag collects diagnostic lines under the global line cap; every line is escaped and clipped.
type diag struct {
	lines   []string
	omitted int
}

func (d *diag) add(format string, args ...any) {
	if len(d.lines) == maxDiagLines {
		d.omitted++
		return
	}
	d.lines = append(d.lines, clip(escapeText(fmt.Sprintf(format, args...)), maxSummaryLine))
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
	out = append(out, d.lines...)
	if d.omitted > 0 {
		out = append(out, fmt.Sprintf("... %d more lines omitted (see compact.log)", d.omitted))
	}

	out = append(out,
		fmt.Sprintf("files: full=%s compact=%s raw=%s stderr=%s", path(fullFile), path(compactFile), path(rawFile), path(stderrFile)),
		fmt.Sprintf("drill down: grep -n '^#<id> ' %s", path(fullFile)))
	if c.lossy > 0 {
		out = append(out, fmt.Sprintf("incomplete lines: %d (truncated or gap); raw records: sed -n '%sp' %s", c.lossy, c.firstLossy, path(rawFile)))
	}
	return strings.Join(out, "\n") + "\n"
}

func (c *capture) diagnostics(d *diag, res result) {
	if h := res.stderr; h != nil && h.total > 0 {
		d.add("STDERR %d lines", h.total)
		for _, line := range h.lines {
			d.add("  %s", line)
		}
	}
	if p := c.pkgs[textPkg]; !c.sawEvent && res.goExit != 0 && p != nil {
		d.add("OUTPUT TAIL (no test2json events)")
		for _, line := range p.tail {
			d.add("  %s", line)
		}
	}

	for _, path := range c.builds {
		d.add("BUILD FAIL %s", path)
		for _, line := range c.buildLines[path] {
			d.add("  %s", line)
		}
	}

	for _, b := range c.failBlocks {
		d.add("FAIL %s %s (#%d..#%d)", b.pkg, b.name, b.first, b.last)
		for _, line := range b.lines {
			d.add("  %s", line)
		}
		for _, line := range b.events {
			d.add("  %s", line)
		}
	}
	if c.moreFailed > 0 {
		d.add("... %d more failed tests not shown (grep -e '--- FAIL' full.log)", c.moreFailed)
	}

	for _, p := range c.failing {
		d.add("CANDIDATES %s (test hints are uncertain; includes passing tests)", p.name)
		for _, cd := range p.cands {
			d.add("  #%d total=%d %s", cd.id, cd.total, cd.text)
		}
		if len(p.cands) == 0 {
			d.add("  (no WRN/ERR/FTL/PNC events)")
		}
		if p.otherWarns > 0 {
			d.add("  ... %d more WRN/ERR/FTL/PNC events in compact.log", p.otherWarns)
		}
		if p.crashed || len(p.incomplete) > 0 {
			d.add("CRASH %s headline: %s", p.name, cmp.Or(p.headline, p.lastLine, "(none)"))
			for _, line := range p.tail {
				d.add("  %s", line)
			}
		}
	}

	for _, p := range c.failing {
		for _, name := range p.incomplete {
			d.add("incomplete: %s %s", p.name, name)
		}
	}
}
