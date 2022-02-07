# Client Example

The Client is useful when working with multiple hostnames.

See the simplest `sendGetRequest()` for GET and more advanced `sendPostRequest()` for a POST request.

The `sendPostRequest()` also shows:
* Per-request timeout with `DoTimeout()`
* Send a body as bytes slice with `SetBodyRaw()`. This is useful if you generated a request body. Otherwise, prefer `SetBody()` which copies it.
* Parse JSON from response
* Gracefully show error messages i.e. timeouts as warnings and other errors as a failures with detailed error messages.

## How to build and run
Start a web server on localhost:8080 then execute:

    make
    ./client

## Client vs HostClient
Internally the Client creates a dedicated HostClient for each domain/IP address and cleans unused after period of time.
So if you have a single heavily loaded API endpoint it's better to use HostClient. See an example in the [examples/host_client](../host_client/)
