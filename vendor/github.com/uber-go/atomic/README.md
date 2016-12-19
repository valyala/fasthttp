# atomic [![GoDoc][doc-img]][doc] [![Build Status][ci-img]][ci] [![Coverage Status][cov-img]][cov]

Simple numeric wrappers to enforce atomic access.

## Installation
`go get -u github.com/uber-go/atomic`

## Usage
The standard library's `sync/atomic` is powerful, but it's easy to forget which
variables must be accessed atomically. `uber-go/atomic` preserves all the
functionality of the standard library, but wraps the primitive numeric types to
provide a safer, more convenient API.

```go
var atom atomic.Uint32
atom.Store(42)
atom.Sub(2)
atom.CAS(40, 11)
```

See the [documentation][doc] for a complete API specification.

## Development Status
Stable.

<hr>
Released under the [MIT License](LICENSE.txt).

[doc-img]: https://godoc.org/github.com/uber-go/atomic?status.svg
[doc]: https://godoc.org/github.com/uber-go/atomic
[ci-img]: https://travis-ci.org/uber-go/atomic.svg?branch=master
[ci]: https://travis-ci.org/uber-go/atomic
[cov-img]: https://coveralls.io/repos/github/uber-go/atomic/badge.svg?branch=master
[cov]: https://coveralls.io/github/uber-go/atomic?branch=master
