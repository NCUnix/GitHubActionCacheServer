package response

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

type Payload interface {
	responsePayload()
}

type JSONResponse[T Payload] interface {
	StatusCode() int
	Body() T
}

type TextResponse interface {
	StatusCode() int
	Text() string
}

type EmptyResponse interface {
	StatusCode() int
	Headers() map[string]string
}

type jsonResponse[T Payload] struct {
	status  int
	payload T
}

func (r jsonResponse[T]) StatusCode() int {
	return r.status
}

func (r jsonResponse[T]) Body() T {
	return r.payload
}

type textResponse struct {
	status int
	body   string
}

func (r textResponse) StatusCode() int {
	return r.status
}

func (r textResponse) Text() string {
	return r.body
}

type emptyResponse struct {
	status  int
	headers map[string]string
}

func (r emptyResponse) StatusCode() int {
	return r.status
}

func (r emptyResponse) Headers() map[string]string {
	return r.headers
}

type basePayload struct {
	OK bool `json:"ok"`
}

func (basePayload) responsePayload() {}

type EmptyPayload struct {
	basePayload
}

type CreateCacheEntryPayload struct {
	basePayload

	SignedUploadURL string `json:"signed_upload_url"`
}

type GetCacheEntryDownloadURLPayload struct {
	basePayload

	SignedDownloadURL string `json:"signed_download_url"`
	MatchedKey        string `json:"matched_key"`
}

type FinalizeCacheEntryUploadPayload struct {
	basePayload

	EntryID string `json:"entry_id"`
}

type ErrorPayload struct {
	basePayload

	Error string `json:"error"`
}

type TwirpErrorPayload struct {
	Code TwirpCode `json:"code"`
	Msg  string    `json:"msg"`
}

func (TwirpErrorPayload) responsePayload() {}

type TwirpCode string

const (
	TwirpMalformed        TwirpCode = "malformed"
	TwirpInvalidArgument  TwirpCode = "invalid_argument"
	TwirpUnauthenticated  TwirpCode = "unauthenticated"
	TwirpPermissionDenied TwirpCode = "permission_denied"
	TwirpNotFound         TwirpCode = "not_found"
	TwirpAlreadyExists    TwirpCode = "already_exists"
	TwirpInternal         TwirpCode = "internal"
	TwirpUnavailable      TwirpCode = "unavailable"
)

// HTTPStatus returns the HTTP status the Twirp spec assigns to code.
func (code TwirpCode) HTTPStatus() int {
	switch code {
	case TwirpMalformed, TwirpInvalidArgument:
		return http.StatusBadRequest
	case TwirpUnauthenticated:
		return http.StatusUnauthorized
	case TwirpPermissionDenied:
		return http.StatusForbidden
	case TwirpNotFound:
		return http.StatusNotFound
	case TwirpAlreadyExists:
		return http.StatusConflict
	case TwirpUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

const InternalServerErrorMessage = "internal server error"

func JSON[T Payload](c *gin.Context, response JSONResponse[T]) {
	c.JSON(response.StatusCode(), response.Body())
}

func Text(c *gin.Context, response TextResponse) {
	c.String(response.StatusCode(), response.Text())
}

func Empty(c *gin.Context, response EmptyResponse) {
	for key, value := range response.Headers() {
		c.Header(key, value)
	}
	c.Status(response.StatusCode())
}

func PlainOK(body string) TextResponse {
	return textResponse{
		status: http.StatusOK,
		body:   body,
	}
}

func CacheMiss() JSONResponse[EmptyPayload] {
	return jsonResponse[EmptyPayload]{
		status:  http.StatusOK,
		payload: EmptyPayload{basePayload: basePayload{OK: false}},
	}
}

func CreateCacheEntry(uploadURL string) JSONResponse[CreateCacheEntryPayload] {
	return jsonResponse[CreateCacheEntryPayload]{
		status: http.StatusOK,
		payload: CreateCacheEntryPayload{
			basePayload:     basePayload{OK: true},
			SignedUploadURL: uploadURL,
		},
	}
}

func GetCacheEntryDownloadURL(downloadURL, matchedKey string) JSONResponse[GetCacheEntryDownloadURLPayload] {
	return jsonResponse[GetCacheEntryDownloadURLPayload]{
		status: http.StatusOK,
		payload: GetCacheEntryDownloadURLPayload{
			basePayload:       basePayload{OK: true},
			SignedDownloadURL: downloadURL,
			MatchedKey:        matchedKey,
		},
	}
}

func FinalizeCacheEntryUpload(entryID string) JSONResponse[FinalizeCacheEntryUploadPayload] {
	return jsonResponse[FinalizeCacheEntryUploadPayload]{
		status: http.StatusOK,
		payload: FinalizeCacheEntryUploadPayload{
			basePayload: basePayload{OK: true},
			EntryID:     entryID,
		},
	}
}

func AzureCreated(requestID string) EmptyResponse {
	return emptyResponse{
		status: http.StatusCreated,
		headers: map[string]string{
			"x-ms-request-id": requestID,
		},
	}
}

func Error(status int, message string) JSONResponse[ErrorPayload] {
	return jsonResponse[ErrorPayload]{
		status: status,
		payload: ErrorPayload{
			basePayload: basePayload{OK: false},
			Error:       message,
		},
	}
}

// InternalError records the private cause for the request logger and sends a
// response that does not expose internal details.
func InternalError(c *gin.Context, err error) {
	RecordInternalError(c, err)
	JSON(c, Error(http.StatusInternalServerError, InternalServerErrorMessage))
}

func RecordInternalError(c *gin.Context, err error) {
	c.Error(err).SetType(gin.ErrorTypePrivate)
}

func NotImplemented() JSONResponse[ErrorPayload] {
	return Error(http.StatusNotImplemented, "not implemented")
}

func TwirpError(code TwirpCode, message string) JSONResponse[TwirpErrorPayload] {
	return jsonResponse[TwirpErrorPayload]{
		status:  code.HTTPStatus(),
		payload: TwirpErrorPayload{Code: code, Msg: message},
	}
}

// TwirpInternalError is InternalError for Twirp routes.
func TwirpInternalError(c *gin.Context, err error) {
	RecordInternalError(c, err)
	JSON(c, TwirpError(TwirpInternal, InternalServerErrorMessage))
}
