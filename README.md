# LoggerBench Challenge

A Go-based device simulator and benchmark for logger-platform APIs. It runs conformance checks, generates device load, and audits stored readings.

```sh
go run . -url http://localhost:8080 -devices 5000 -duration 5m
```

See `go run . -h` for options. Licensed under the [MIT License](LICENSE).

See [template.md](template.md) for the challenge specification.
