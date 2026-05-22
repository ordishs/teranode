package httpimpl

import (
	"io"

	"github.com/labstack/echo/v4"
)

// streamOrAbort is a drop-in replacement for echo.Context.Stream that prevents
// truncated bodies from being cached by a reverse proxy (nginx in particular)
// when the source reader fails mid-stream.
//
// Background — the bug we're closing
//
// echo.Context.Stream commits to a 200 OK status line the moment the first
// byte is read from the source reader, then io.Copy's the rest of the body
// to the response. If the source reader fails partway through (e.g. the
// on-demand subtreeData generation in services/asset/repository can't find
// some tx in the local store), io.Copy returns the error and the handler
// returns. So far so good — except Go's net/http server, in finishRequest(),
// then unconditionally writes the chunked-transfer terminator "0\r\n\r\n"
// to close the response cleanly. Wire-syntactically the chunked stream
// looks complete. A caching reverse proxy in front of the asset service
// (nginx's proxy_cache) takes that at face value and persists the truncated
// body to the cache. Every subsequent client request hits the bad cache
// entry, and the catchup loop loses the race against the corruption.
//
// # Fix
//
// On the happy path, behave exactly like c.Stream — the chunked terminator
// is written normally and nginx caches the response as today. On the
// failure path, hijack the underlying TCP connection and close it
// **without** writing the terminator. nginx detects "upstream prematurely
// closed connection while reading upstream", refuses to commit the partial
// response to cache (this is the same path nginx already uses for any
// truncated chunked response from upstream, documented behaviour), and the
// next client request goes back to the asset service for a fresh attempt.
//
// # Caller contract
//
// streamOrAbort always returns nil to its echo caller, even on streaming
// failure — once we hijack the connection we own it, and any further
// writes from echo's after-handler middleware would either fail or escape
// our control. Returning nil to echo signals "response already finalised,
// don't touch it." The error from io.Copy is consumed inside this helper
// (an abrupt hijack + close is sufficient signalling to the client, which
// will surface the truncation as io.ErrUnexpectedEOF in its body parse).
//
// The Hijacker assertion succeeds in production: echo's *Response wraps
// the standard library's http.ResponseWriter, which implements Hijacker
// on both HTTP/1.x and HTTP/2 (via http2.responseWriter). If for any
// reason the cast fails, we fall back to letting echo's normal error
// path run — this is strictly safer than panicking, even if it means
// the original cache-corruption bug can re-occur on that exotic path.
func streamOrAbort(c echo.Context, code int, contentType string, r io.Reader) error {
	h := c.Response().Header()
	h.Set(echo.HeaderContentType, contentType)
	c.Response().WriteHeader(code)

	_, copyErr := io.Copy(c.Response(), r)
	if copyErr == nil {
		// Happy path — let echo / Go finalise the chunked stream normally.
		return nil
	}

	// Mid-stream failure. Hijack the connection and close it before Go's
	// HTTP server has a chance to write the chunked terminator. nginx then
	// sees an incomplete chunked response and discards its cache file.
	if conn, _, hjErr := c.Response().Hijack(); hjErr == nil {
		_ = conn.Close()
		return nil
	}

	// Hijack unsupported — fall back to surfacing the io.Copy error so
	// echo can run its normal error path. In this case the cache-bypass
	// guarantee no longer holds, but this branch should never fire under
	// the standard echo + net/http stack in production.
	return copyErr
}
