# Zero-allocation server example

* Displays the same request info as [helloworldserver](../helloworldserver), but with 0 allocs/op.
* Avoids `fmt` and zero-value structs in the request path. See [issue #1764](https://github.com/valyala/fasthttp/issues/1764).

# How to build

```
make
```

# How to run

```
./zeroallocserver -addr=tcp.addr.to.listen:to
```

# How to verify the zero-allocation claim

```
go test -run TestZeroAllocation -v .
```
