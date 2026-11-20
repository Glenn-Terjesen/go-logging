// Command ailog runs `go test -json`, keeps the complete output in a private run
// directory and prints a bounded summary that points into it.
//
// Usage: ailog [-ailog.out=PARENT] [go test args...]
package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

const (
	outFlag      = "-ailog.out"
	exitFailure  = 2
	filePerm     = 0o600
	stderrLines  = 10
	writeBufSize = 64 << 10
)

const (
	rawFile     = "events.jsonl"
	stderrFile  = "go.stderr"
	fullFile    = "full.log"
	compactFile = "compact.log"
)

// options holds what the tests replace; main uses the real process environment.
type options struct {
	stdout io.Writer
	dir    string // child working directory, "" for the current one
	create func(dir, name string) (io.WriteCloser, error)
}

func main() {
	os.Exit(run(os.Args[1:], options{stdout: os.Stdout, create: createFile}))
}

func run(args []string, opts options) int {
	parent, userArgs, err := parseArgs(args)
	if err != nil {
		say(opts.stdout, "AILOG ERROR: %v\n", err)
		return exitFailure
	}

	runDir, err := os.MkdirTemp(parent, "ailog-")
	if err != nil {
		say(opts.stdout, "AILOG ERROR: create run dir: %v\n", err)
		return exitFailure
	}

	argv := append([]string{"test", "-json", "-count=1"}, userArgs...)
	say(opts.stdout, "AILOG run=%s\ncmd: go %s\n", runDir, shellJoin(argv))

	failure := &latch{}
	sinks := map[string]*sink{}
	for _, name := range []string{rawFile, stderrFile, fullFile, compactFile} {
		f, err := opts.create(runDir, name)
		if err != nil {
			closeSinks(sinks)
			say(opts.stdout, "AILOG ERROR: create %s: %v\n", name, err)
			return exitFailure
		}
		sinks[name] = newSink(name, f, failure)
	}

	cmd := exec.Command("go", argv...) //nolint:gosec // running the go tool with the user's own test arguments is the purpose
	cmd.Dir = opts.dir
	stdout, err := cmd.StdoutPipe()
	if err == nil {
		var stderr io.ReadCloser
		stderr, err = cmd.StderrPipe()
		if err == nil {
			err = cmd.Start()
		}
		if err == nil {
			return finish(cmd, stdout, stderr, sinks, failure, opts, runDir)
		}
	}
	closeSinks(sinks)
	say(opts.stdout, "AILOG ERROR: start go: %v\n", err)
	return exitFailure
}

func finish(cmd *exec.Cmd, stdout, stderr io.Reader, sinks map[string]*sink, failure *latch, opts options, runDir string) int {
	head := &headLines{limit: stderrLines}
	var wg sync.WaitGroup
	wg.Go(func() {
		if _, err := io.Copy(io.MultiWriter(sinks[stderrFile], head), stderr); err != nil {
			failure.set(fmt.Errorf("read stderr: %w", err))
		}
	})

	c := newCapture(sinks[rawFile], sinks[fullFile], sinks[compactFile], cwdPrefix(opts.dir))
	if err := c.consume(stdout); err != nil {
		failure.set(fmt.Errorf("read stdout: %w", err))
		_, _ = io.Copy(io.Discard, stdout) // keep the child from blocking on a full pipe
	}
	wg.Wait()
	head.close()

	goExit := 0
	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			failure.set(fmt.Errorf("wait for go: %w", err))
		}
		goExit = cmd.ProcessState.ExitCode()
	}
	c.finish()
	closeSinks(sinks)

	captureErr := failure.get()
	say(opts.stdout, "%s", c.report(result{
		runDir:     runDir,
		goExit:     goExit,
		captureErr: captureErr,
		stderr:     head,
	}))

	switch {
	case captureErr != nil, goExit < 0:
		return exitFailure
	default:
		return goExit
	}
}

// parseArgs consumes -ailog.out before the first -args and forwards everything else in order.
func parseArgs(args []string) (parent string, rest []string, err error) {
	rest = make([]string, 0, len(args))
	for len(args) > 0 {
		arg := args[0]
		args = args[1:]
		switch {
		case arg == "-args":
			return parent, append(append(rest, arg), args...), nil
		case strings.HasPrefix(arg, outFlag+"="):
			parent = strings.TrimPrefix(arg, outFlag+"=")
		case arg == outFlag:
			if len(args) == 0 {
				return "", nil, fmt.Errorf("%s needs a directory", outFlag)
			}
			parent, args = args[0], args[1:]
		default:
			rest = append(rest, arg)
		}
	}
	return parent, rest, nil
}

func createFile(dir, name string) (io.WriteCloser, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
	if closeErr := root.Close(); err == nil && closeErr != nil {
		_ = f.Close()
		return nil, closeErr
	}
	return f, err
}

func cwdPrefix(dir string) string {
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return ""
		}
		dir = wd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(filepath.ToSlash(abs), "/") + "/"
}

func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = arg
		if arg == "" || strings.ContainsFunc(arg, needsShellQuote) {
			quoted[i] = "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
		}
	}
	return strings.Join(quoted, " ")
}

func needsShellQuote(r rune) bool {
	safe := 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || strings.ContainsRune("-_./=:,+@%", r)
	return !safe
}

func say(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...) // nothing useful to do if our own stdout is gone
}

// latch keeps the first capture error; once set, every sink discards.
type latch struct {
	mu  sync.Mutex
	err error
}

func (l *latch) set(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err == nil {
		l.err = err
	}
}

func (l *latch) get() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// sink is a buffered file writer that never fails its caller: errors are latched
// so that the pipes keep draining and the child can finish.
type sink struct {
	name    string
	f       io.WriteCloser
	w       *bufio.Writer
	failure *latch
}

func newSink(name string, f io.WriteCloser, failure *latch) *sink {
	return &sink{name: name, f: f, w: bufio.NewWriterSize(f, writeBufSize), failure: failure}
}

func (s *sink) Write(p []byte) (int, error) {
	s.write(p)
	return len(p), nil
}

func (s *sink) write(p []byte) {
	if s.failure.get() != nil {
		return
	}
	if _, err := s.w.Write(p); err != nil {
		s.failure.set(fmt.Errorf("write %s: %w", s.name, err))
	}
}

func (s *sink) printf(format string, args ...any) {
	s.write(fmt.Appendf(nil, format, args...))
}

func (s *sink) close() {
	if err := s.w.Flush(); err != nil {
		s.failure.set(fmt.Errorf("write %s: %w", s.name, err))
	}
	if err := s.f.Close(); err != nil {
		s.failure.set(fmt.Errorf("close %s: %w", s.name, err))
	}
}

func closeSinks(sinks map[string]*sink) {
	for _, s := range sinks {
		s.close()
	}
}

// headLines keeps the first few lines written to it, each bounded, and counts the rest.
type headLines struct {
	limit   int
	lines   []string
	partial []byte
	total   int
}

func (h *headLines) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		chunk := p
		if i >= 0 {
			chunk = p[:i]
		}
		if room := maxSummaryLine - len(h.partial); len(h.lines) < h.limit && room > 0 {
			h.partial = append(h.partial, chunk[:min(room, len(chunk))]...)
		}
		if i < 0 {
			break
		}
		h.endLine()
		p = p[i+1:]
	}
	return n, nil
}

func (h *headLines) endLine() {
	if len(h.lines) < h.limit {
		h.lines = append(h.lines, string(h.partial))
	}
	h.partial = h.partial[:0]
	h.total++
}

// close counts an unterminated last line.
func (h *headLines) close() {
	if len(h.partial) > 0 {
		h.endLine()
	}
}
