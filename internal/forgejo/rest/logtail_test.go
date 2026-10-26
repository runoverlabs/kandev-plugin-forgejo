package rest

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"kandev-plugin-forgejo/internal/scm"

	"github.com/stretchr/testify/require"
)

// TestJobLogTailUsesSuffixRange checks the happy path: the server honors the
// suffix range and truncation happens before the bytes cross the network.
func TestJobLogTailUsesSuffixRange(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.HandleFunc("GET", "/api/v1/repos/kandev/demo/actions/jobs/42/logs", func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "bytes=-16", r.Header.Get("Range"))
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("0123456789abcdef"))
	})

	tail, truncated, err := newTestClient(t, api).JobLogTail(context.Background(), "kandev", "demo", 42, 16)
	require.NoError(t, err)
	require.Equal(t, "0123456789abcdef", tail)
	require.True(t, truncated, "a full suffix range means there was more before it")
}

// TestJobLogTailIgnoredRangeStillTails is the important one: a host that
// ignores Range must not make the plugin buffer or return the head of the log.
func TestJobLogTailIgnoredRangeStillTails(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	body := strings.Repeat("x", 5000) + "THE-END"
	api.HandleFunc("GET", "/api/v1/repos/kandev/demo/actions/jobs/42/logs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(body))
	})

	tail, truncated, err := newTestClient(t, api).JobLogTail(context.Background(), "kandev", "demo", 42, 32)
	require.NoError(t, err)
	require.Len(t, tail, 32)
	require.True(t, strings.HasSuffix(tail, "THE-END"))
	require.True(t, truncated)
}

// TestJobLogTailShortLogIsNotTruncated guards the off-by-one at the boundary.
func TestJobLogTailShortLogIsNotTruncated(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.HandleFunc("GET", "/api/v1/repos/kandev/demo/actions/jobs/42/logs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("short log\n"))
	})

	tail, truncated, err := newTestClient(t, api).JobLogTail(context.Background(), "kandev", "demo", 42, 1024)
	require.NoError(t, err)
	require.Equal(t, "short log\n", tail)
	require.False(t, truncated)
}

// TestJobLogTailMissingEndpoint covers Forgejo 13, which serves runs but has no
// job log endpoint at all.
func TestJobLogTailMissingEndpoint(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	_, _, err := newTestClient(t, api).JobLogTail(context.Background(), "kandev", "demo", 42, 1024)
	require.ErrorIs(t, err, scm.ErrNotFound)
}

func TestAppendTail(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name  string
		tail  string
		next  string
		limit int
		want  string
	}{
		{name: "fits", tail: "ab", next: "cd", limit: 8, want: "abcd"},
		{name: "exact", tail: "ab", next: "cd", limit: 4, want: "abcd"},
		{name: "overflows", tail: "ab", next: "cde", limit: 4, want: "bcde"},
		{name: "chunk alone exceeds", tail: "ab", next: "cdefgh", limit: 4, want: "efgh"},
		{name: "empty tail", tail: "", next: "abc", limit: 2, want: "bc"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := appendTail([]byte(testCase.tail), []byte(testCase.next), testCase.limit)
			require.Equal(t, testCase.want, string(got))
			require.LessOrEqual(t, len(got), testCase.limit)
		})
	}
}

// TestReadTailAcrossManyChunks exercises the streaming path with more data
// than one read returns, which is where a ring-buffer bug would hide.
func TestReadTailAcrossManyChunks(t *testing.T) {
	t.Parallel()
	var builder strings.Builder
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(&builder, "line %d\n", i)
	}
	tail, total, err := readTail(strings.NewReader(builder.String()), 64)
	require.NoError(t, err)
	require.Len(t, tail, 64)
	require.Equal(t, int64(builder.Len()), total)
	require.True(t, strings.HasSuffix(string(tail), "line 19999\n"))
}

// TestJobLogTailNormalizesGiteaServerError pins a measured host difference:
// Gitea 1.24.7 answers an unknown job id with 500 while Forgejo 16.0.5 answers
// 404, and both mean the same thing.
func TestJobLogTailNormalizesGiteaServerError(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.HandleFunc("GET", "/api/v1/repos/kandev/demo/actions/jobs/42/logs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"run job with id 42: resource does not exist"}`))
	})

	_, _, err := newTestClient(t, api).JobLogTail(context.Background(), "kandev", "demo", 42, 1024)
	require.ErrorIs(t, err, scm.ErrNotFound)
}

// TestJobLogTailKeepsOtherFailuresDistinct keeps that normalization narrow: a
// bad gateway in front of the instance is not a missing job.
func TestJobLogTailKeepsOtherFailuresDistinct(t *testing.T) {
	t.Parallel()
	api := newAPIServer(t)
	api.HandleFunc("GET", "/api/v1/repos/kandev/demo/actions/jobs/42/logs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})

	_, _, err := newTestClient(t, api).JobLogTail(context.Background(), "kandev", "demo", 42, 1024)
	require.NotErrorIs(t, err, scm.ErrNotFound)
	var status *StatusError
	require.ErrorAs(t, err, &status)
	require.Equal(t, http.StatusTooManyRequests, status.Status)
}
