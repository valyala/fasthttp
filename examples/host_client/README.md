# Host Client Example

The HostClient is useful when calling an API from a single host.
The example also shows how to use URI.
You may create the parsed URI once and reuse it in many requests.
The URI has a username and password for Basic Auth but you may also set other parts i.e. `SetPath()`, `SetQueryString()`.

# How to build and run
Start a web server on localhost:8080 then execute:

    make
    ./host_client

