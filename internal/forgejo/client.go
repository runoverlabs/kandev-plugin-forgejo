// Package forgejo is the concrete provider adapter layer. It owns every
// Forgejo/Gitea-specific URL, pagination token, authentication detail, and
// error mapping, and exposes only the narrow ports the provider-neutral
// source-control recipe declares.
//
// Forgejo is a hard fork of Gitea and both serve the same versioned REST
// surface at <base>/api/v1. This client deliberately restricts itself to
// endpoints and fields that exist on both, so one plugin serves either host.
package forgejo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxResponseBytes bounds any single API response the plugin will buffer. A
// misconfigured or hostile base_url must not be able to exhaust plugin memory.
const maxResponseBytes = 8 << 20

// ErrNotFound is returned for 404 and 410 responses. Callers translate it into
// "not owned by this connection" rather than surfacing it as a transport error.
var ErrNotFound = errors.New("forgejo: resource not found")

// ErrUnauthorized is returned for 401/403. It never carries the response body,
// which on some deployments echoes the presented token.
var ErrUnauthorized = errors.New("forgejo: not authorized")

// StatusError carries an unexpected HTTP status without the response body,
// which on some deployments echoes the presented token. Callers that need to
// tell one failure mode from another read Status; everything else treats it as
// an opaque transport error.
type StatusError struct {
	Method string
	Path   string
	Status int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("forgejo: %s %s: unexpected status %d", e.Method, e.Path, e.Status)
}

// Client is a bounded REST v1 client for one Forgejo or Gitea instance.
type Client struct {
	baseURL *url.URL
	token   string
	http    *http.Client
}

// NewClient validates baseURL and returns a client for the instance it names.
// The token is kept in memory only; it is never logged or echoed.
func NewClient(baseURL, token string, httpClient *http.Client) (*Client, error) {
	normalized, err := NormalizeBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	parsed, err := url.Parse(normalized)
	if err != nil {
		return nil, fmt.Errorf("forgejo: parse instance URL: %w", err)
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("forgejo: an access token is required")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{baseURL: parsed, token: strings.TrimSpace(token), http: httpClient}, nil
}

// NormalizeBaseURL canonicalizes an operator-supplied instance URL into the
// value used as the connection scope. It requires an absolute http(s) URL and
// strips any query, fragment, and trailing slash so that the same instance
// always produces the same scope string.
func NormalizeBaseURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("forgejo: instance URL is required")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("forgejo: parse instance URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("forgejo: instance URL must be http or https, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", errors.New("forgejo: instance URL must include a host")
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.Path = strings.TrimSuffix(parsed.Path, "/")
	return parsed.String(), nil
}

// Scope is the connection scope for this client: the normalized instance URL.
func (c *Client) Scope() string { return c.baseURL.String() }

// Host is the instance hostname, used as the repository provider host.
func (c *Client) Host() string { return c.baseURL.Host }

// apiV1 is the versioned REST surface Forgejo and Gitea share.
const apiV1 = "/api/v1"

// apiForgejoV1 is Forgejo's own API namespace. Gitea does not serve it, which
// makes it a reliable flavor probe.
const apiForgejoV1 = "/api/forgejo/v1"

// get issues an authenticated GET against /api/v1<path> and decodes JSON into
// out. query may be nil.
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	return c.do(ctx, http.MethodGet, apiV1, path, query, nil, out)
}

// post issues an authenticated POST against /api/v1<path>.
func (c *Client) post(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPost, apiV1, path, nil, body, out)
}

// put issues an authenticated PUT against /api/v1<path>. Like delete, it
// classifies failures into *WriteError.
func (c *Client) put(ctx context.Context, path string, body, out any) error {
	return c.write(ctx, http.MethodPut, path, nil, body, out)
}

// delete issues an authenticated DELETE against /api/v1<path>. body may be nil;
// the reviewer endpoints take one.
func (c *Client) delete(ctx context.Context, path string, body any) error {
	return c.write(ctx, http.MethodDelete, path, nil, body, nil)
}

// newRequest builds one authenticated request against <base><prefix><path>.
func (c *Client) newRequest(ctx context.Context, method, prefix, path string, query url.Values, body any) (*http.Request, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// `path` arrives already percent-escaped (see pathSegment). url.URL.Path
	// holds the DECODED path and re-escapes '%' on String(), so the escaped
	// form must go in RawPath and the decoded form in Path — otherwise a
	// repository named "a/b" is requested as "a%252Fb".
	endpoint := *c.baseURL
	escapedPath := strings.TrimSuffix(endpoint.EscapedPath(), "/") + prefix + path
	decodedPath, err := url.PathUnescape(escapedPath)
	if err != nil {
		return nil, fmt.Errorf("forgejo: build request path: %w", err)
	}
	endpoint.Path = decodedPath
	endpoint.RawPath = escapedPath
	if query != nil {
		endpoint.RawQuery = query.Encode()
	}

	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("forgejo: encode request body: %w", err)
		}
		payload = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), payload)
	if err != nil {
		return nil, fmt.Errorf("forgejo: build request: %w", err)
	}
	// Forgejo and Gitea both accept the "token <value>" scheme for personal
	// access tokens on every REST v1 endpoint.
	request.Header.Set("Authorization", "token "+c.token)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return request, nil
}

func (c *Client) do(ctx context.Context, method, prefix, path string, query url.Values, body, out any) error {
	return c.send(ctx, method, prefix, path, query, body, out, false)
}

// write is do for mutations whose failures callers must tell apart: a 403 or
// 409 becomes a *WriteError with a fixed Reason instead of being flattened.
func (c *Client) write(ctx context.Context, method, path string, query url.Values, body, out any) error {
	return c.send(ctx, method, apiV1, path, query, body, out, true)
}

func (c *Client) send(ctx context.Context, method, prefix, path string, query url.Values, body, out any, classify bool) error {
	request, err := c.newRequest(ctx, method, prefix, path, query, body)
	if err != nil {
		return err
	}
	response, err := c.http.Do(request)
	if err != nil {
		// Wrap without the URL's userinfo and without the token header.
		return fmt.Errorf("forgejo: %s %s: %w", method, path, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
		_ = response.Body.Close()
	}()

	switch {
	case response.StatusCode == http.StatusNotFound, response.StatusCode == http.StatusGone:
		return ErrNotFound
	case response.StatusCode == http.StatusUnauthorized:
		return ErrUnauthorized
	case response.StatusCode == http.StatusForbidden && !classify:
		return ErrUnauthorized
	case response.StatusCode >= 400 && classify:
		// The body is read only to classify and is never returned or logged.
		raw, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBodyBytes))
		return &WriteError{Status: response.StatusCode, Reason: classifyWrite(response.StatusCode, string(raw))}
	case response.StatusCode >= 400:
		return &StatusError{Method: method, Path: path, Status: response.StatusCode}
	}

	if out == nil {
		return nil
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes))
	if err := decoder.Decode(out); err != nil {
		if classify && errors.Is(err, io.EOF) {
			// A mutation may legitimately answer 200 or 204 with no body.
			return nil
		}
		return fmt.Errorf("forgejo: decode %s response: %w", path, err)
	}
	return nil
}

// patch issues an authenticated PATCH against /api/v1<path>.
func (c *Client) patch(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPatch, apiV1, path, nil, body, out)
}

// getTail issues an authenticated GET against /api/v1<path> and returns at most
// maxBytes from the END of the response body, plus whether anything was
// dropped.
//
// CI logs are the one response this plugin reads that has no useful bound, and
// the interesting part of a failing job is always the end. A suffix Range is
// requested first because it moves the truncation to the server; a host that
// ignores it still works, because the body is streamed through a tail buffer
// rather than materialized.
func (c *Client) getTail(ctx context.Context, path string, query url.Values, maxBytes int) (string, bool, error) {
	if maxBytes <= 0 {
		return "", false, errors.New("forgejo: tail size must be positive")
	}
	request, err := c.newRequest(ctx, http.MethodGet, apiV1, path, query, nil)
	if err != nil {
		return "", false, err
	}
	request.Header.Set("Accept", "text/plain")
	request.Header.Set("Range", fmt.Sprintf("bytes=-%d", maxBytes))

	response, err := c.http.Do(request)
	if err != nil {
		return "", false, fmt.Errorf("forgejo: GET %s: %w", path, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
		_ = response.Body.Close()
	}()

	switch {
	case response.StatusCode == http.StatusNotFound, response.StatusCode == http.StatusGone:
		return "", false, ErrNotFound
	case response.StatusCode == http.StatusUnauthorized, response.StatusCode == http.StatusForbidden:
		return "", false, ErrUnauthorized
	case response.StatusCode >= 400:
		return "", false, &StatusError{Method: http.MethodGet, Path: path, Status: response.StatusCode}
	}

	tail, read, err := readTail(io.LimitReader(response.Body, maxResponseBytes), maxBytes)
	if err != nil {
		return "", false, fmt.Errorf("forgejo: read %s: %w", path, err)
	}
	// A 206 means the server already applied the suffix range, so whatever
	// arrived is the true tail and "truncated" means the log was longer.
	truncated := read > int64(len(tail))
	if response.StatusCode == http.StatusPartialContent {
		truncated = len(tail) >= maxBytes
	}
	return string(tail), truncated, nil
}

// readTail streams reader and keeps only the last maxBytes, reporting how many
// bytes went past. It never holds more than maxBytes plus one chunk.
func readTail(reader io.Reader, maxBytes int) ([]byte, int64, error) {
	tail := make([]byte, 0, maxBytes)
	chunk := make([]byte, 32<<10)
	var total int64
	for {
		n, err := reader.Read(chunk)
		if n > 0 {
			total += int64(n)
			tail = appendTail(tail, chunk[:n], maxBytes)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return tail, total, nil
			}
			return nil, total, err
		}
	}
}

// appendTail appends next to tail, discarding from the front so the result
// never exceeds maxBytes.
func appendTail(tail, next []byte, maxBytes int) []byte {
	if len(next) >= maxBytes {
		return append(tail[:0], next[len(next)-maxBytes:]...)
	}
	if overflow := len(tail) + len(next) - maxBytes; overflow > 0 {
		tail = append(tail[:0], tail[overflow:]...)
	}
	return append(tail, next...)
}

// getHead issues an authenticated GET against /api/v1<path> and returns at most
// maxBytes from the START of the response body, plus whether anything was
// dropped. It is the head-bounded counterpart of getTail, for diffs, where the
// first files are the ones a reader wants and the rest can be paged or dropped.
func (c *Client) getHead(ctx context.Context, path string, query url.Values, maxBytes int) (string, bool, error) {
	if maxBytes <= 0 {
		return "", false, errors.New("forgejo: head size must be positive")
	}
	request, err := c.newRequest(ctx, http.MethodGet, apiV1, path, query, nil)
	if err != nil {
		return "", false, err
	}
	request.Header.Set("Accept", "text/plain")

	response, err := c.http.Do(request)
	if err != nil {
		return "", false, fmt.Errorf("forgejo: GET %s: %w", path, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
		_ = response.Body.Close()
	}()

	switch {
	case response.StatusCode == http.StatusNotFound, response.StatusCode == http.StatusGone:
		return "", false, ErrNotFound
	case response.StatusCode == http.StatusUnauthorized, response.StatusCode == http.StatusForbidden:
		return "", false, ErrUnauthorized
	case response.StatusCode >= 400:
		return "", false, &StatusError{Method: http.MethodGet, Path: path, Status: response.StatusCode}
	}

	// Read one byte past the cap to learn whether the body was longer.
	head, err := io.ReadAll(io.LimitReader(response.Body, int64(maxBytes)+1))
	if err != nil {
		return "", false, fmt.Errorf("forgejo: read %s: %w", path, err)
	}
	if len(head) > maxBytes {
		return string(head[:maxBytes]), true, nil
	}
	return string(head), false, nil
}
