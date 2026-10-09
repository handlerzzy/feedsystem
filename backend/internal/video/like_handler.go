package video

import (
	"github.com/handlerzzy/feedsystem/internal/apierror"
	"github.com/handlerzzy/feedsystem/internal/middleware/jwt"

	"github.com/gin-gonic/gin"
)

type LikeHandler struct {
	service *LikeService
}

func NewLikeHandler(service *LikeService) *LikeHandler {
	return &LikeHandler{service: service}
}

func (lh *LikeHandler) Like(c *gin.Context) {
	var req LikeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierror.Respond(c, err)
		return
	}
	if req.VideoID <= 0 {
		c.JSON(400, gin.H{"error": "video_id is required"})
		return
	}

	accountID, err := jwt.GetAccountID(c)
	if err != nil {
		apierror.Respond(c, err)
		return
	}

	like := &Like{
		VideoID:   req.VideoID,
		AccountID: accountID,
	}
	if err := lh.service.Like(c.Request.Context(), like); err != nil {
		apierror.Respond(c, err)
		return
	}
	c.JSON(200, gin.H{"message": "like success"})
}

func (lh *LikeHandler) Unlike(c *gin.Context) {
	var req LikeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierror.Respond(c, err)
		return
	}
	if req.VideoID <= 0 {
		c.JSON(400, gin.H{"error": "video_id is required"})
		return
	}

	accountID, err := jwt.GetAccountID(c)
	if err != nil {
		apierror.Respond(c, err)
		return
	}

	like := &Like{
		VideoID:   req.VideoID,
		AccountID: accountID,
	}
	if err := lh.service.Unlike(c.Request.Context(), like); err != nil {
		apierror.Respond(c, err)
		return
	}
	c.JSON(200, gin.H{"message": "unlike success"})
}

func (lh *LikeHandler) IsLiked(c *gin.Context) {
	var req LikeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierror.Respond(c, err)
		return
	}
	if req.VideoID <= 0 {
		c.JSON(400, gin.H{"error": "video_id is required"})
		return
	}

	accountID, err := jwt.GetAccountID(c)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	isLiked, err := lh.service.IsLiked(c.Request.Context(), req.VideoID, accountID)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	c.JSON(200, gin.H{"is_liked": isLiked})
}

func (lh *LikeHandler) ListMyLikedVideos(c *gin.Context) {
	accountID, err := jwt.GetAccountID(c)
	if err != nil {
		apierror.Respond(c, err)
		return
	}

	videos, err := lh.service.ListLikedVideos(c.Request.Context(), accountID)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	if videos == nil {
		videos = []Video{}
	}
	c.JSON(200, videos)
}
