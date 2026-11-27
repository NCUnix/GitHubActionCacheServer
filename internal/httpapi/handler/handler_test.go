package handler

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/NCUnix/GitHubActionCacheServer/internal/cache"
	"github.com/NCUnix/GitHubActionCacheServer/internal/testutil"
	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func TestWriteCacheErrorHidesWrappedDetails(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/cache", nil)
	handler := &Handler{logger: zerolog.Nop()}

	handler.writeCacheError(c, fmt.Errorf("%w: sensitive storage detail", cache.ErrPartCountMismatch))

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.JSONEq(
		t,
		`{"ok":false,"error":"uploaded part count does not match actual part count in storage"}`,
		recorder.Body.String(),
	)
	require.NotContains(t, recorder.Body.String(), "sensitive storage detail")
}

func TestCacheErrorWritersShareClassification(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		wantMsg    string
	}{
		{"no write scope", cache.ErrNoWriteScope, http.StatusForbidden, "permission_denied", "no scope with write permission found"},
		{"upload already exists", cache.ErrUploadAlreadyExists, http.StatusConflict, "already_exists", "cache entry with the same key, version, and scope already exists"},
		{"upload not found", cache.ErrUploadNotFound, http.StatusNotFound, "not_found", "upload not found"},
		{"cache not found", cache.ErrCacheNotFound, http.StatusNotFound, "not_found", "cache not found"},
		{"no parts uploaded", cache.ErrNoPartsUploaded, http.StatusBadRequest, "invalid_argument", "no parts have been uploaded"},
		{"part count mismatch", cache.ErrPartCountMismatch, http.StatusBadRequest, "invalid_argument", "uploaded part count does not match actual part count in storage"},
		{"block list too large", cache.ErrBlockListTooLarge, http.StatusBadRequest, "invalid_argument", "block list exceeds 50000 entries"},
		{"internal", errors.New("database down"), http.StatusInternalServerError, "internal", "internal server error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := fmt.Errorf("%w: sensitive storage detail", tt.err)
			handler := &Handler{logger: zerolog.Nop()}
			wantPrivateErrors := 0
			if tt.wantCode == "internal" {
				wantPrivateErrors = 1
			}

			legacy, legacyContext := newCacheErrorTestContext()
			handler.writeCacheError(legacyContext, err)
			require.Equal(t, tt.wantStatus, legacy.Code)
			require.JSONEq(t, fmt.Sprintf(`{"ok":false,"error":%q}`, tt.wantMsg), legacy.Body.String())
			require.NotContains(t, legacy.Body.String(), "sensitive storage detail")
			require.Len(t, legacyContext.Errors.ByType(gin.ErrorTypePrivate), wantPrivateErrors)

			twirp, twirpContext := newCacheErrorTestContext()
			handler.writeTwirpCacheError(twirpContext, err)
			require.Equal(t, tt.wantStatus, twirp.Code)
			require.JSONEq(t, fmt.Sprintf(`{"code":%q,"msg":%q}`, tt.wantCode, tt.wantMsg), twirp.Body.String())
			require.NotContains(t, twirp.Body.String(), "sensitive storage detail")
			require.Len(t, twirpContext.Errors.ByType(gin.ErrorTypePrivate), wantPrivateErrors)
		})
	}
}

func TestReadyReportsUnavailableDatabase(t *testing.T) {
	_, client := testutil.NewSQLiteClient(t)
	handler := New(Options{DB: client})
	require.NoError(t, client.Close())
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/readyz", nil)

	handler.Ready(c)

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.Equal(t, "database unavailable", recorder.Body.String())
}

func newCacheErrorTestContext() (*httptest.ResponseRecorder, *gin.Context) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/cache", nil)
	return recorder, c
}
