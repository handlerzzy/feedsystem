package social

import (
	"github.com/handlerzzy/feedsystem/internal/account"
	"github.com/handlerzzy/feedsystem/internal/apierror"
	"github.com/handlerzzy/feedsystem/internal/middleware/jwt"
	"net/http"

	"github.com/gin-gonic/gin"
)

type SocialHandler struct {
	service *SocialService
}

func NewSocialHandler(service *SocialService) *SocialHandler {
	return &SocialHandler{service: service}
}

func (h *SocialHandler) Follow(c *gin.Context) {
	var req FollowRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierror.Respond(c, err)
		return
	}
	if req.VloggerID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "vlogger_id is required"})
		return
	}
	FollowerID, err := jwt.GetAccountID(c)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	social := &Social{
		FollowerID: FollowerID,
		VloggerID:  req.VloggerID,
	}
	if err := h.service.Follow(c.Request.Context(), social); err != nil {
		apierror.Respond(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "followed"})
}

func (h *SocialHandler) Unfollow(c *gin.Context) {
	var req UnfollowRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierror.Respond(c, err)
		return
	}
	if req.VloggerID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "vlogger_id is required"})
		return
	}
	FollowerID, err := jwt.GetAccountID(c)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	social := &Social{
		FollowerID: FollowerID,
		VloggerID:  req.VloggerID,
	}
	if err := h.service.Unfollow(c.Request.Context(), social); err != nil {
		apierror.Respond(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "unfollowed"})
}

func (h *SocialHandler) GetAllFollowers(c *gin.Context) {
	var req GetAllFollowersRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierror.Respond(c, err)
		return
	}

	vloggerID := req.VloggerID
	if vloggerID == 0 {
		accountID, err := jwt.GetAccountID(c)
		if err != nil {
			apierror.Respond(c, err)
			return
		}
		vloggerID = accountID
	}

	followers, err := h.service.GetAllFollowers(c.Request.Context(), vloggerID)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	if followers == nil {
		followers = []*account.PublicAccount{}
	}
	followerCount, _ := h.service.CountFollowers(c.Request.Context(), vloggerID)
	c.JSON(http.StatusOK, GetAllFollowersResponse{Followers: followers, FollowerCount: followerCount})
}

func (h *SocialHandler) GetAllVloggers(c *gin.Context) {
	var req GetAllVloggersRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierror.Respond(c, err)
		return
	}

	followerID := req.FollowerID
	if followerID == 0 {
		accountID, err := jwt.GetAccountID(c)
		if err != nil {
			apierror.Respond(c, err)
			return
		}
		followerID = accountID
	}

	vloggers, err := h.service.GetAllVloggers(c.Request.Context(), followerID)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	if vloggers == nil {
		vloggers = []*account.PublicAccount{}
	}
	vloggerCount, _ := h.service.CountVloggers(c.Request.Context(), followerID)
	c.JSON(http.StatusOK, GetAllVloggersResponse{Vloggers: vloggers, VloggerCount: vloggerCount})
}

func (h *SocialHandler) GetCounts(c *gin.Context) {
	accountID, err := jwt.GetAccountID(c)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	followerCount, _ := h.service.CountFollowers(c.Request.Context(), accountID)
	vloggerCount, _ := h.service.CountVloggers(c.Request.Context(), accountID)
	c.JSON(http.StatusOK, SocialCounts{FollowerCount: followerCount, VloggerCount: vloggerCount})
}
