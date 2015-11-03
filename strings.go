package fasthttp

var (
	defaultServerName  = []byte("fasthttp server")
	defaultContentType = []byte("text/plain; charset=utf-8")
)

var (
	strSlash            = []byte("/")
	strSlashSlash       = []byte("//")
	strSlashDotDot      = []byte("/..")
	strSlashDotDotSlash = []byte("/../")
	strCRLF             = []byte("\r\n")
	strHTTP             = []byte("http")
	strHTTP11           = []byte("HTTP/1.1")
	strColonSlashSlash  = []byte("://")
	strColonSpace       = []byte(": ")

	strGet  = []byte("GET")
	strHead = []byte("HEAD")
	strPost = []byte("POST")

	strConnection       = []byte("Connection")
	strContentLength    = []byte("Content-Length")
	strContentType      = []byte("Content-Type")
	strDate             = []byte("Date")
	strHost             = []byte("Host")
	strReferer          = []byte("Referer")
	strServer           = []byte("Server")
	strTransferEncoding = []byte("Transfer-Encoding")
	strUserAgent        = []byte("User-Agent")
	strCookie           = []byte("Cookie")

	strClose               = []byte("close")
	strChunked             = []byte("chunked")
	strPostArgsContentType = []byte("application/x-www-form-urlencoded")
)
