# HelloWorld server example

* Displays various request info with 0 allocs/op.
* Avoids `fmt` and zero-value structs in the request path. See [issue #1764](https://github.com/valyala/fasthttp/issues/1764).
* Sets response headers and cookies.
* Supports transparent compression. Note that compression itself allocates - the
  0 allocs/op claim covers the uncompressed path.

# How to build

```
make
```

# How to run

```
./helloworldserver -addr=tcp.addr.to.listen:to
```

# How to verify the zero-allocation claim

```
go test -run TestZeroAllocation -v .
```
