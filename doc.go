/*
Package fasthttp provides fast HTTP server and client API.

Fasthttp provides the following features:

    * Optimized for speed. Fasthttp is faster than standard net/http package.
    * Optimized for low memory usage.
    * Easily handles more than 1M concurrent keep-alive connections on modern
      hardware.
    * Server supports requests' pipelining. Multiple requests may be read from
      a single network packet and multiple responses may be sent in a single
      network packet. This may be useful for highly loaded REST services.
    * Server is packed with the following anti-DoS limits:

        * The number of concurrent connections.
        * The number of concurrent connections per client IP.
        * The number of requests per connection.
        * Request read timeout.
        * Response write timeout.
        * Maximum request header size.

    * A lot of useful info is exposed to request handler:

        * Server and client address.
        * Per-request logger.
        * Unique request id.
        * Request start time.
        * Connection start time.
        * Request sequence number for the current connection.

    * Client supports automatic retry on idempotent requests' failure.
    * It is quite easy building custom (and fast) client and server
      implementations using fasthttp API.
*/
package fasthttp
