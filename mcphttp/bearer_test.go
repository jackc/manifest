package mcphttp_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/manifest/mcphttp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

const (
	origin   = "https://example.com"
	metadata = "https://example.com/.well-known/oauth-protected-resource"
	scoped   = `Bearer resource_metadata="https://example.com/.well-known/oauth-protected-resource", scope="mcp"`
)

type userKey struct{}

var errBadToken = errors.New("invalid or expired access token")

// verify accepts "good" and "limited" as users of those names, and "narrow"
// as a valid token lacking the scope.
func verify(ctx context.Context, token string) (context.Context, error) {
	switch token {
	case "good", "limited":
		return context.WithValue(ctx, userKey{}, token), nil
	case "narrow":
		return nil, mcphttp.InsufficientScope(errors.New("access token requires the mcp scope"))
	}
	return nil, errBadToken
}

// limiter refuses "limited" and records whom it counted.
type limiter struct{ counted []string }

func (l *limiter) limitPost(ctx context.Context) time.Duration {
	user := ctx.Value(userKey{}).(string)
	l.counted = append(l.counted, user)
	if user == "limited" {
		return 1500 * time.Millisecond
	}
	return 0
}

func config(l *limiter) mcphttp.Bearer {
	return mcphttp.Bearer{Origin: origin, ResourceMetadataURL: metadata, Scope: "mcp", Verify: verify, LimitPost: l.limitPost}
}

// echoUser reports the verified user and the request body it could read.
var echoUser = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return
	}
	if err != nil {
		panic(err)
	}
	fmt.Fprintf(w, "%s:%s", r.Context().Value(userKey{}), body)
})

func serve(h http.Handler, method, authorization string, headers http.Header, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/mcp", strings.NewReader(body))
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	for name, values := range headers {
		r.Header[name] = values
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestOriginIsCheckedBeforeCredentials(t *testing.T) {
	h := mcphttp.RequireBearer(config(&limiter{}))(echoUser)
	for _, tc := range []struct {
		name    string
		origins []string
		pass    bool
	}{
		{"absent", nil, true},
		{"configured", []string{origin}, true},
		{"other", []string{"https://evil.example"}, false},
		{"trailing slash", []string{origin + "/"}, false},
		{"other case", []string{"https://Example.com"}, false},
		{"empty", []string{""}, false},
		{"repeated", []string{origin, origin}, false},
	} {
		for _, method := range []string{"GET", "POST", "DELETE", "OPTIONS"} {
			t.Run(tc.name+" "+method, func(t *testing.T) {
				for _, authorization := range []string{"Bearer good", ""} {
					w := serve(h, method, authorization, http.Header{"Origin": tc.origins}, "")
					switch {
					case !tc.pass:
						require.Equal(t, http.StatusForbidden, w.Code)
						require.Equal(t, "invalid origin\n", w.Body.String())
						require.Equal(t, "text/plain; charset=utf-8", w.Header().Get("Content-Type"))
						require.Empty(t, w.Header().Get("WWW-Authenticate"))
					case authorization == "":
						require.Equal(t, http.StatusUnauthorized, w.Code)
					default:
						require.Equal(t, http.StatusOK, w.Code)
					}
				}
			})
		}
	}
}

func TestChallenges(t *testing.T) {
	h := mcphttp.RequireBearer(config(&limiter{}))(echoUser)
	for _, tc := range []struct {
		name, authorization string
		status              int
		challenge, body     string
	}{
		{"no credentials", "", 401, scoped, `{"error":"unauthorized"}`},
		{"other scheme", "Basic Z29vZA==", 401, scoped, `{"error":"unauthorized"}`},
		{"scheme without token", "Bearer", 401, scoped, `{"error":"unauthorized"}`},
		{"tab separator", "Bearer\tgood", 401, scoped, `{"error":"unauthorized"}`},
		{"empty token", "Bearer   ", 401,
			scoped + `, error="invalid_token", error_description="missing bearer token"`,
			`{"error":"unauthorized","error_description":"missing bearer token"}`},
		{"invalid token", "Bearer bad", 401,
			scoped + `, error="invalid_token", error_description="invalid or expired access token"`,
			`{"error":"unauthorized","error_description":"invalid or expired access token"}`},
		{"insufficient scope", "Bearer narrow", 403,
			scoped + `, error="insufficient_scope", error_description="access token requires the mcp scope"`,
			`{"error":"insufficient_scope","error_description":"access token requires the mcp scope"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := serve(h, "POST", tc.authorization, nil, "{}")
			require.Equal(t, tc.status, w.Code)
			require.Equal(t, tc.challenge, w.Header().Get("WWW-Authenticate"))
			require.Equal(t, "application/json", w.Header().Get("Content-Type"))
			require.Equal(t, tc.body+"\n", w.Body.String())
			require.Empty(t, w.Header().Get("Cache-Control"))
		})
	}
}

func TestChallengeDescriptions(t *testing.T) {
	var got []string
	cfg := config(&limiter{})
	cfg.Scope = ""
	cfg.Verify = func(_ context.Context, token string) (context.Context, error) {
		got = append(got, token)
		switch token {
		case "wrapped":
			return nil, fmt.Errorf("token lacks %q: %w", "mcp", mcphttp.ErrInsufficientScope)
		case "quoted":
			return nil, errors.New("bad \"token\"\r\nX-Injected: 1 <&>")
		}
		return nil, errors.New("bad \xff")
	}
	h := mcphttp.RequireBearer(cfg)(echoUser)

	w := serve(h, "GET", "", nil, "")
	require.Equal(t, `Bearer resource_metadata="https://example.com/.well-known/oauth-protected-resource"`, w.Header().Get("WWW-Authenticate"))

	// Verify receives the token after the scheme, scheme case ignored,
	// without surrounding whitespace.
	w = serve(h, "GET", "bEaReR   wrapped \t", nil, "")
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Equal(t, `Bearer resource_metadata="https://example.com/.well-known/oauth-protected-resource", error="insufficient_scope", error_description="token lacks \"mcp\": insufficient scope"`, w.Header().Get("WWW-Authenticate"))
	require.Equal(t, `{"error":"insufficient_scope","error_description":"token lacks \"mcp\": insufficient scope"}`+"\n", w.Body.String())

	// Descriptions are quoted in the header and escaped in the body.
	w = serve(h, "GET", "Bearer quoted", nil, "")
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Equal(t, `Bearer resource_metadata="https://example.com/.well-known/oauth-protected-resource", error="invalid_token", error_description="bad \"token\"\r\nX-Injected: 1 <&>"`, w.Header().Get("WWW-Authenticate"))
	require.Empty(t, w.Header().Get("X-Injected"))
	require.Equal(t, `{"error":"unauthorized","error_description":"bad \"token\"\r\nX-Injected: 1 <&>"}`+"\n", w.Body.String())

	w = serve(h, "GET", "Bearer invalid-utf8", nil, "")
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Equal(t, `{"error":"unauthorized","error_description":"bad �"}`+"\n", w.Body.String())

	require.Equal(t, []string{"wrapped", "quoted", "invalid-utf8"}, got)
}

func TestInsufficientScopeKeepsTheError(t *testing.T) {
	cause := errors.New("access token requires the mcp scope")
	err := mcphttp.InsufficientScope(cause)
	require.Equal(t, cause.Error(), err.Error())
	require.ErrorIs(t, err, mcphttp.ErrInsufficientScope)
	require.ErrorIs(t, err, cause)
	require.NotErrorIs(t, cause, mcphttp.ErrInsufficientScope)
	require.PanicsWithValue(t, "mcphttp: nil insufficient scope error", func() { mcphttp.InsufficientScope(nil) })
}

func TestVerifiedContextReachesTheEndpoint(t *testing.T) {
	type requestKey struct{}
	cfg := config(&limiter{})
	cfg.Verify = func(ctx context.Context, token string) (context.Context, error) {
		// Verify runs with the request's context.
		require.Equal(t, "request", ctx.Value(requestKey{}))
		return context.WithValue(ctx, userKey{}, token), nil
	}
	h := mcphttp.RequireBearer(cfg)(echoUser)
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader("body"))
	r = r.WithContext(context.WithValue(r.Context(), requestKey{}, "request"))
	r.Header.Set("Authorization", "Bearer alice")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "alice:body", w.Body.String())
}

func TestNilVerifiedContextIsAnInternalError(t *testing.T) {
	cfg := config(&limiter{})
	cfg.Verify = func(context.Context, string) (context.Context, error) { return nil, nil }
	called := false
	h := mcphttp.RequireBearer(cfg)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	w := serve(h, "POST", "Bearer good", nil, "")
	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.Equal(t, `{"error":"internal error"}`+"\n", w.Body.String())
	require.False(t, called)
}

func TestAuthenticatedPostsAreRateLimited(t *testing.T) {
	l := &limiter{}
	h := mcphttp.RequireBearer(config(l))(echoUser)

	require.Equal(t, http.StatusOK, serve(h, "POST", "Bearer good", nil, "").Code)
	// Other methods and unauthenticated requests are not counted.
	require.Equal(t, http.StatusOK, serve(h, "GET", "Bearer limited", nil, "").Code)
	require.Equal(t, http.StatusOK, serve(h, "DELETE", "Bearer limited", nil, "").Code)
	require.Equal(t, http.StatusUnauthorized, serve(h, "POST", "Bearer bad", nil, "").Code)
	require.Equal(t, http.StatusForbidden, serve(h, "POST", "Bearer limited", http.Header{"Origin": {"https://evil.example"}}, "").Code)

	w := serve(h, "POST", "Bearer limited", nil, "")
	require.Equal(t, http.StatusTooManyRequests, w.Code)
	require.Equal(t, "2", w.Header().Get("Retry-After"))
	require.Equal(t, "application/json", w.Header().Get("Content-Type"))
	require.Equal(t, `{"error":"rate_limit_exceeded","error_description":"too many requests; retry later"}`+"\n", w.Body.String())
	require.Equal(t, []string{"good", "limited"}, l.counted)

	for retry, header := range map[time.Duration]string{time.Nanosecond: "1", time.Second: "1", time.Minute + 1: "61"} {
		cfg := config(&limiter{})
		cfg.LimitPost = func(context.Context) time.Duration { return retry }
		w := serve(mcphttp.RequireBearer(cfg)(echoUser), "POST", "Bearer good", nil, "")
		require.Equal(t, http.StatusTooManyRequests, w.Code)
		require.Equal(t, header, w.Header().Get("Retry-After"), retry)
	}
	cfg := config(&limiter{})
	cfg.LimitPost = func(context.Context) time.Duration { return -time.Second }
	require.Equal(t, http.StatusOK, serve(mcphttp.RequireBearer(cfg)(echoUser), "POST", "Bearer good", nil, "").Code)
}

func TestOptionalNoStoreAndBodyLimit(t *testing.T) {
	const limit = 8
	plain := mcphttp.RequireBearer(config(&limiter{}))(echoUser)
	cfg := config(&limiter{})
	cfg.NoStore = true
	cfg.MaxBodyBytes = limit
	guarded := mcphttp.RequireBearer(cfg)(echoUser)

	large := strings.Repeat("x", limit+1)
	w := serve(plain, "POST", "Bearer good", nil, large)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "good:"+large, w.Body.String())
	require.Empty(t, w.Header().Get("Cache-Control"))

	w = serve(guarded, "POST", "Bearer good", nil, strings.Repeat("x", limit))
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	w = serve(guarded, "POST", "Bearer good", nil, large)
	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	require.Equal(t, "no-store", w.Header().Get("Cache-Control"))

	// Refusals precede both options.
	for _, w := range []*httptest.ResponseRecorder{
		serve(guarded, "POST", "", nil, large),
		serve(guarded, "POST", "Bearer bad", nil, large),
		serve(guarded, "POST", "Bearer good", http.Header{"Origin": {"https://evil.example"}}, large),
	} {
		require.NotEqual(t, http.StatusOK, w.Code)
		require.Empty(t, w.Header().Get("Cache-Control"))
	}
}

func TestInvalidConfigurationPanics(t *testing.T) {
	for _, tc := range []struct {
		name   string
		modify func(*mcphttp.Bearer)
		panic  string
	}{
		{"no origin", func(b *mcphttp.Bearer) { b.Origin = "" }, "origin"},
		{"origin with slash", func(b *mcphttp.Bearer) { b.Origin = origin + "/" }, "origin"},
		{"origin with path", func(b *mcphttp.Bearer) { b.Origin = origin + "/mcp" }, "origin"},
		{"origin with query", func(b *mcphttp.Bearer) { b.Origin = origin + "?x" }, "origin"},
		{"origin with fragment", func(b *mcphttp.Bearer) { b.Origin = origin + "#x" }, "origin"},
		{"origin with credentials", func(b *mcphttp.Bearer) { b.Origin = "https://user@example.com" }, "origin"},
		{"origin with uppercase scheme", func(b *mcphttp.Bearer) { b.Origin = "HTTPS://example.com" }, "origin"},
		{"origin with uppercase host", func(b *mcphttp.Bearer) { b.Origin = "https://Example.com" }, "origin"},
		{"origin with default https port", func(b *mcphttp.Bearer) { b.Origin = origin + ":443" }, "origin"},
		{"origin with default http port", func(b *mcphttp.Bearer) { b.Origin = "http://example.com:80" }, "origin"},
		{"origin with empty port", func(b *mcphttp.Bearer) { b.Origin = origin + ":" }, "origin"},
		{"origin with other scheme", func(b *mcphttp.Bearer) { b.Origin = "ftp://example.com" }, "origin"},
		{"relative origin", func(b *mcphttp.Bearer) { b.Origin = "example.com" }, "origin"},
		{"no metadata URL", func(b *mcphttp.Bearer) { b.ResourceMetadataURL = "" }, "resource metadata"},
		{"relative metadata URL", func(b *mcphttp.Bearer) { b.ResourceMetadataURL = "/.well-known/oauth-protected-resource" }, "resource metadata"},
		{"metadata URL with other scheme", func(b *mcphttp.Bearer) { b.ResourceMetadataURL = "ftp://example.com/x" }, "resource metadata"},
		{"unparsable metadata URL", func(b *mcphttp.Bearer) { b.ResourceMetadataURL = "https://example.com/%zz" }, "resource metadata"},
		{"no Verify", func(b *mcphttp.Bearer) { b.Verify = nil }, "mcphttp: Verify is required"},
		{"no LimitPost", func(b *mcphttp.Bearer) { b.LimitPost = nil }, "mcphttp: LimitPost is required"},
		{"negative body limit", func(b *mcphttp.Bearer) { b.MaxBodyBytes = -1 }, "mcphttp: negative body limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config(&limiter{})
			tc.modify(&cfg)
			defer func() {
				p := recover()
				require.NotNil(t, p)
				require.Contains(t, p, tc.panic)
			}()
			mcphttp.RequireBearer(cfg)
		})
	}
	for _, valid := range []string{"http://localhost:8080", "http://127.0.0.1:3000", "http://[::1]:3000", "https://example.com:8443"} {
		cfg := config(&limiter{})
		cfg.Origin = valid
		require.NotPanics(t, func() { mcphttp.RequireBearer(cfg) }, valid)
	}
}

func TestStreamableHTTPOptions(t *testing.T) {
	opts := mcphttp.StreamableHTTPOptions()
	require.Equal(t, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, PropagateRequestCancellation: true, DisableLocalhostProtection: true}, opts)
	opts.JSONResponse = false
	require.True(t, mcphttp.StreamableHTTPOptions().JSONResponse, "each call returns its own options")
}

// Behind a proxy that connects over loopback with the public Host header, the
// guarded SDK handler serves tools.
func TestGuardedHandlerServesBehindALoopbackProxy(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "whoami"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct {
		User string `json:"user"`
	}, error) {
		return nil, struct {
			User string `json:"user"`
		}{ctx.Value(userKey{}).(string)}, nil
	})
	guarded := func(opts *mcp.StreamableHTTPOptions) http.Handler {
		return mcphttp.RequireBearer(config(&limiter{}))(
			mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, opts))
	}
	call := func(h http.Handler) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"whoami","arguments":{}}}`))
		r.Host = "example.com"
		r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8080}))
		r.Header.Set("Authorization", "Bearer good")
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		r.Header.Set("Mcp-Protocol-Version", "2025-06-18")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	w := call(guarded(mcphttp.StreamableHTTPOptions()))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, "application/json", w.Header().Get("Content-Type"))
	require.Contains(t, w.Body.String(), `"structuredContent":{"user":"good"}`)

	// The SDK's own protection would refuse the proxied request.
	opts := mcphttp.StreamableHTTPOptions()
	opts.DisableLocalhostProtection = false
	require.Equal(t, http.StatusForbidden, call(guarded(opts)).Code)
}
