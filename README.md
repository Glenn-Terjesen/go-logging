# go-logging

Go-Logging is intended as a simple-to-use SDK for high-performance logging in GCP and locally. It supports caller location identification, optional stacktraces, colorful logging and more.

## Quickstart

### Install 
```sh
go get github.com/entur/go-logging
go mod tidy
```

### Basic Usage 
```golang
import (
  "github.com/entur/go-logging"
)

func main() {
  // Global logger
  logging.Warn().Msg("Starting up my application")

  // Formatted logging
  logging.Warn().Msgf("Starting my %s", "application")

  // Local logger
  logger := logging.New()
  logger.Warn().Msg("Starting up my application")

  // Local logger with custom level
  logger = logging.New(
    logging.WithLevel(logging.DebugLevel)
  )
  logger.Debug().Msg("Starting up my application")
}
```

## Overview

### Log Levels
The default log level will be derived from the `LOG_LEVEL` environment variable if defined, or set to the `warning` level if not as per Entur's [Architecture Descision Record](https://enturas.atlassian.net/wiki/spaces/eat/pages/5318344894/2022-10-31+All+services+must+have+a+balanced+log+level). 

Valid environment variable values for log levels are:
* `fatal` `ftl`
* `panic` `pnc`
* `error` `err`
* `warning` `wrn`
* `info` `inf`
* `debug` `dbg`
* `trace` `trc`

Log levels can also be specified for on logger instance creation like so:
```go
func main() {
  // New logger with its own level
  logger = logging.New(
    logging.WithLevel(logging.DebugLevel)
  )

  // Child logger with its own level again
  additionalLogger := logger.Level(logging.TraceLevel)
}
```

### Stack Traces
The Go-Logging SDK supports logging of errors with stacktraces. To do so, simply create a new or wrap an existing error using the `logging.NewStackTraceError()` before you dispatch it for logging.

```go
import (
  "fmt"

  "github.com/entur/go-logging"
)

func newErr() error {
  err := logging.NewStackTraceError("called newErr()")
  return err
}

func wrappedErr() error {
  existingErr := fmt.Errorf("called wrappedErr()")
  err := logging.NewStackTraceError("%w", existingErr) // Stack traces will be retrieved at the point NewStackTraceError is called
  return err
}

func main() {
  err := newErr()
  logging.Error().Err(err).Msg("An internal error occurred")

  err = wrappedErr()
  logging.Error().Err(err).Msg("An internal error occurred")
}
```

### Calling Location Info
The Go-Logging SDK will automatically include the caller info when you use the global logging functions, or create a new instanced logger. If you want to disable the feature, you can prove the `WithNoCaller()` option to the logging constructor.

```go
import (
  "github.com/entur/go-logging"
)

func main() {
  // Disable caller info in child logger
  logger := logging.New(
    logging.WithNoCaller()
  )

  logger.Debug().Msg("This log won't include caller info")

  // You can still include caller info individually, even if it is disabled by default
  logger.Debug().Caller("This log will include caller info")
}
```

## Test runs for AI agents (`ailog`)

`cmd/ailog` is a small, standard-library-only wrapper around `go test -json`. It keeps the complete test output on disk and prints a bounded summary, so a coding agent can see the result, the failures and the warnings logged around them without reading the whole stream. It does not change production logging: it only reads what tests already write.

### Install
```sh
go get -tool github.com/entur/go-logging/cmd/ailog
go tool ailog ./...
```

`ailog [-ailog.out=PARENT] [go test args...]` runs `go test -json -count=1 ARGS...`; your own flags come later and win (for example `-count=2`). `-ailog.out` is only read before `-args`. The exit code is the one from `go test`, or `2` if the capture itself failed.

### AGENTS.md / CLAUDE.md snippet
```text
Run Go tests with `go tool ailog ./...` (go test flags work as usual); read the whole summary, do not pipe it through head/tail.
`?TestName` on a log line is only a hint: global log output cannot be attributed to a test reliably; check the CANDIDATES warnings too.
Details: `grep -n '^#<id> ' <full.log>` for a line ID from the summary, or search the printed compact.log.
```

### Files
Every run gets a new private directory (`AILOG run=<dir>`, under `-ailog.out` or the system temp dir):

| File | Content |
|---|---|
| `events.jsonl` | `go test -json` stdout, byte for byte |
| `go.stderr` | `go test` stderr |
| `full.log` | every reassembled output line as `#N <package> ?<test hint> <line>` |
| `compact.log` | the same lines, zerolog JSON rendered as `#N WRN pkg ?Test at=file.go:12 msg="..." k=v`, adjacent repeats collapsed |

`#N` is assigned when a line is completed, so it follows completion order per package, not a global timeline across packages. Lines longer than 1 MiB are truncated in `full.log` and point to their raw records (`raw_records=START:END`, line numbers in `events.jsonl`). The files are not size-limited; a noisy suite can use a lot of disk.

### Attribution
Go reports output from global loggers, background goroutines and parallel tests under whichever test last printed, so the `?Test` hint can name the wrong (even a passing) test. For exact per-test output, inject a test-scoped logger into the code under test:

```go
logger := logging.New(logging.WithWriter(t.Output()))
```

## Tests
This project makes use of Example tests. To run them, simply use use the following command
```sh
go test ./...
```

## Examples
Interested in seeing how Go-Logging is used in practice at Entur? Take a look at the following repositories:
* [https://github.com/entur/go-orchestrator](https://github.com/entur/go-orchestrator)