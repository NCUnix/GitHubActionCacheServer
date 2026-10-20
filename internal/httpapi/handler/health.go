package handler

import (
	"github.com/NCUnix/GitHubActionCacheServer/internal/httpapi/response"
	"github.com/gin-gonic/gin"
)

func Root(c *gin.Context) {
	response.Text(c, response.PlainOK("OK"))
}

func Health(c *gin.Context) {
	response.Text(c, response.PlainOK("healthy"))
}
