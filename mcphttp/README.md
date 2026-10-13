# MCP over HTTP

`mcphttp` guards an MCP Streamable HTTP endpoint as an OAuth protected resource.
It imports neither application code nor other Manifest packages. It is neither
an authorization server nor a token format: the application verifies tokens,
and owns the identity they carry and the limiter that counts requests.

## Bearer middleware

`RequireBearer(Bearer{...})` returns a `func(http.Handler) http.Handler`. It is
not an `httpregistry.Stage`, so an application wraps it in one, naming what it
installs:

```go
guard := mcphttp.RequireBearer(mcphttp.Bearer{
    Origin:              canonicalURL,
    ResourceMetadataURL: canonicalURL + "/.well-known/oauth-protected-resource",
    Scope:               "mcp",
    Verify:              verify,
    LimitPost:           func(ctx context.Context) time.Duration { return limiter.allow(userID(ctx)) },
})
stage := httpregistry.Stage{Name: "mcp-origin-bearer-and-post-rate-limit", Wrap: guard}
```

Each request passes these checks in order; the first failure answers it:

1. **Origin.** A request with an `Origin` header must send exactly one, equal
   to `Origin`; otherwise it receives 403 with the plain-text body
   `invalid origin`. Every method is checked, before credentials. Clients
   other than browsers may omit the header.
2. **Credentials** (RFC 6750). The `Authorization` header must hold a scheme
   matching `Bearer` case-insensitively, one space, and a token, which is
   trimmed of surrounding whitespace before `Verify` sees it. Without one,
   the response is 401 with `WWW-Authenticate: Bearer
   resource_metadata="…", scope="…"` (scope only when set) and the body
   `{"error":"unauthorized"}`, naming no error code, since the client
   presented no token. An empty token, or a `Verify` error, adds
   `error="invalid_token"` and `error_description` to the challenge, and the
   description to the body. An error matching `ErrInsufficientScope` instead
   receives 403 with `insufficient_scope` as both codes.
3. **Rate limit.** `LimitPost` is called with the verified context for every
   authenticated POST, which includes every JSON-RPC call; other methods are
   not counted. A positive delay refuses the request with 429, `Retry-After`
   in whole seconds (rounded up, at least 1), and the body
   `{"error":"rate_limit_exceeded","error_description":"too many requests; retry later"}`.
   The limiter keys on the verified identity, so no header or token rotation
   resets the allowance.
4. **Options.** `NoStore` sets `Cache-Control: no-store`. A positive
   `MaxBodyBytes` caps the request body with `http.MaxBytesReader`; reading
   past it fails with `*http.MaxBytesError`. Neither applies to refusals.

The endpoint then runs with the context `Verify` returned. JSON bodies are
encoded with `encoding/json/v2` and end with a newline; invalid UTF-8 in an
error description is replaced rather than refused. `Verify` returning neither
a context nor an error is a wiring bug: the request receives 500 with
`{"error":"internal error"}`, and the endpoint does not run.

`Verify`'s error message is sent to the client verbatim, in the header and the
body, so it must be safe to disclose. An application logs unexpected failures
and returns a generic invalid-token error instead. To mark a valid token that
lacks the scope, return `InsufficientScope(err)`, which keeps `err`'s message,
or any error wrapping `ErrInsufficientScope`.

Invalid configuration panics at construction: an `Origin` that is not a
lowercase `http` or `https` origin as browsers serialize it (no path, trailing
slash, query, fragment, credentials, or default port), a
`ResourceMetadataURL` that is not an absolute `http` or `https` URL, a nil
`Verify` or `LimitPost`, or a negative `MaxBodyBytes`. An endpoint without a
request limit must say so with a `LimitPost` that returns zero.
`InsufficientScope(nil)` also panics.

## Handler options

`StreamableHTTPOptions()` returns new SDK handler options: stateless, JSON
responses, request cancellation propagated to handlers, and the SDK's
localhost DNS-rebinding protection disabled. That protection refuses requests
arriving on a loopback address with another `Host`, which is how a trusted
reverse proxy forwards public requests. Use these options only behind such a
proxy, whose host routing defends against DNS rebinding, and behind
`RequireBearer`, whose Origin check refuses cross-origin browser requests.
Adjust the returned value for tools that stream progress notifications.

## Evidence and boundaries

The HTTP inventory sees the application's stage name, not this
configuration; the name should state what the stage enforces. This package does
not discover or validate client metadata, issue or introspect tokens, serve
the protected resource metadata, or limit requests to other endpoints. An
OAuth authorization server package can supply `Verify` without either package
importing the other.

The tests cover the Origin check across methods, each challenge and its
escaping, the scope error marker, the verified context, the nil-context wiring
bug, which requests are counted and their `Retry-After`, both options and their
absence, every construction panic, and the handler options behind a loopback
proxy with the SDK's server. An architecture check rejects dependencies on
application code and other Manifest packages.
