// Package mcphttp guards an MCP Streamable HTTP endpoint with OAuth bearer
// tokens. It is part of Manifest and imports neither application code nor other
// Manifest packages.
package mcphttp

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrInsufficientScope identifies a valid token that lacks the endpoint's
// scope. A Verify error matching it receives 403 rather than 401.
var ErrInsufficientScope = errors.New("insufficient scope")

// InsufficientScope marks err as a valid token lacking the endpoint's scope,
// keeping err's message as the challenge's error_description.
func InsufficientScope(err error) error {
	if err == nil {
		panic("mcphttp: nil insufficient scope error")
	}
	return insufficientScopeError{err}
}

type insufficientScopeError struct{ err error }

func (e insufficientScopeError) Error() string        { return e.err.Error() }
func (e insufficientScopeError) Unwrap() error        { return e.err }
func (e insufficientScopeError) Is(target error) bool { return target == ErrInsufficientScope }

// Bearer configures RequireBearer. Origin, ResourceMetadataURL, Verify, and
// LimitPost are required; the other fields are optional.
type Bearer struct {
	// Origin is the endpoint's public origin, such as https://example.com.
	// A request with an Origin header must send exactly this value.
	Origin string
	// ResourceMetadataURL locates the protected resource metadata (RFC 9728),
	// named by every challenge's resource_metadata parameter.
	ResourceMetadataURL string
	// Scope, when set, is named by every challenge's scope parameter.
	Scope string
	// Verify checks a token and returns the context the endpoint runs with.
	// Its error message is sent to the client as the challenge's
	// error_description, so it must be safe to disclose. An error matching
	// ErrInsufficientScope receives 403; any other error receives 401.
	Verify func(ctx context.Context, token string) (context.Context, error)
	// LimitPost is called for every authenticated POST with Verify's context.
	// It returns zero to admit the request, otherwise the delay before the
	// client may retry. Other methods are not counted.
	LimitPost func(ctx context.Context) (retryAfter time.Duration)
	// NoStore sets Cache-Control: no-store on authenticated responses.
	NoStore bool
	// MaxBodyBytes, when positive, caps the request body of authenticated
	// requests with http.MaxBytesReader. Zero means no cap.
	MaxBodyBytes int64
}

// RequireBearer returns middleware that admits only requests from the
// configured origin carrying a token Verify accepts. Invalid configuration
// panics.
func RequireBearer(b Bearer) func(http.Handler) http.Handler {
	if !isOrigin(b.Origin) {
		panic("mcphttp: origin must be a lowercase http or https origin without a default port, path, query, or fragment")
	}
	if u, err := url.Parse(b.ResourceMetadataURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		panic("mcphttp: resource metadata URL must be an absolute http or https URL")
	}
	if b.Verify == nil {
		panic("mcphttp: Verify is required")
	}
	if b.LimitPost == nil {
		panic("mcphttp: LimitPost is required")
	}
	if b.MaxBodyBytes < 0 {
		panic("mcphttp: negative body limit")
	}
	challenge := fmt.Sprintf(`Bearer resource_metadata=%q`, b.ResourceMetadataURL)
	if b.Scope != "" {
		challenge += fmt.Sprintf(`, scope=%q`, b.Scope)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Check every method against the configured public origin, even
			// behind a proxy. Non-browser clients may omit Origin entirely.
			if origins := r.Header.Values("Origin"); len(origins) != 0 &&
				(len(origins) != 1 || origins[0] != b.Origin) {
				http.Error(w, "invalid origin", http.StatusForbidden)
				return
			}
			scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
			if !ok || !strings.EqualFold(scheme, "Bearer") {
				writeChallenge(w, challenge, nil)
				return
			}
			token = strings.TrimSpace(token)
			if token == "" {
				writeChallenge(w, challenge, errors.New("missing bearer token"))
				return
			}
			ctx, err := b.Verify(r.Context(), token)
			if err != nil {
				writeChallenge(w, challenge, err)
				return
			}
			if ctx == nil {
				// A wiring bug, not a client error: refuse rather than run
				// the endpoint without the verified identity.
				writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal error"})
				return
			}
			// Count every authenticated POST, including legacy calls whose
			// method is only in the body. No header or token rotation can
			// bypass the allowance LimitPost keys on the verified identity.
			if r.Method == http.MethodPost {
				if retry := b.LimitPost(ctx); retry > 0 {
					w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(retry.Seconds())))))
					writeJSON(w, http.StatusTooManyRequests, errorBody{Error: "rate_limit_exceeded", Description: "too many requests; retry later"})
					return
				}
			}
			if b.NoStore {
				w.Header().Set("Cache-Control", "no-store")
			}
			if b.MaxBodyBytes > 0 {
				r.Body = http.MaxBytesReader(w, r.Body, b.MaxBodyBytes)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// isOrigin reports whether s is serialized as browsers send Origin, so an
// exact comparison can match it.
func isOrigin(s string) bool {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" ||
		u.Scheme+"://"+u.Host != s || strings.ToLower(u.Host) != u.Host {
		return false
	}
	port := u.Port()
	return !(u.Scheme == "https" && port == "443") && !(u.Scheme == "http" && port == "80") &&
		!strings.HasSuffix(u.Host, ":")
}

// writeChallenge points clients at the protected resource metadata. Missing
// credentials receive 401 without an error code; a rejected token receives 401
// with invalid_token, or 403 with insufficient_scope (RFC 6750 §3.1).
func writeChallenge(w http.ResponseWriter, challenge string, err error) {
	status := http.StatusUnauthorized
	body := errorBody{Error: "unauthorized"}
	// An initial challenge without credentials should not claim that the
	// client presented an invalid token.
	if err != nil {
		code := "invalid_token"
		if errors.Is(err, ErrInsufficientScope) {
			status = http.StatusForbidden
			code = "insufficient_scope"
			body.Error = code
		}
		challenge += fmt.Sprintf(`, error=%q, error_description=%q`, code, err.Error())
		body.Description = err.Error()
	}
	w.Header().Set("WWW-Authenticate", challenge)
	writeJSON(w, status, body)
}

type errorBody struct {
	Error       string `json:"error"`
	Description string `json:"error_description,omitzero"`
}

func writeJSON(w http.ResponseWriter, status int, body errorBody) {
	// Two strings cannot fail to encode once invalid UTF-8 is replaced.
	data, _ := json.Marshal(body, jsontext.AllowInvalidUTF8(true))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}
