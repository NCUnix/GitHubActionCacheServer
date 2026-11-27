package handler

import (
	"context"
	"time"

	"github.com/NCUnix/GitHubActionCacheServer/internal/httpapi/response"
	"github.com/gin-gonic/gin"
)

const readinessTimeout = 2 * time.Second

func Root(c *gin.Context) {
	response.Text(c, response.PlainOK("OK"))
}

func Health(c *gin.Context) {
	response.Text(c, response.PlainOK("healthy"))
}

func (h *Handler) Ready(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), readinessTimeout)
	defer cancel()
	if _, err := h.db.ExecContext(ctx, "select 1"); err != nil {
		h.logger.Warn().Err(err).Msg("readiness check failed: database unavailable")
		response.Text(c, response.PlainUnavailable("database unavailable"))
		return
	}
	response.Text(c, response.PlainOK("ready"))
}
