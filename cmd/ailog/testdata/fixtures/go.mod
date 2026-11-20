module example.com/ailogfixtures

go 1.26.0

require github.com/entur/go-logging v0.0.0

require (
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/rs/zerolog v1.35.1 // indirect
	golang.org/x/sys v0.36.0 // indirect
)

replace github.com/entur/go-logging => ../../../..
