package message

import (
	"context"
	"strings"
	"time"

	"github.com/handlerzzy/feedsystem/internal/apierror"
	"github.com/handlerzzy/feedsystem/internal/middleware/jwt"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type Repository struct{ db *gorm.DB }
type Service struct {
	repo        MessageStore
	accountRepo AccountLookup // 用于校验私信接收者是否存在
}
type Handler struct{ service *Service }

func NewRepository(db *gorm.DB) *Repository { return &Repository{db: db} }

// NewService 接收 accountRepo，用于校验私信接收者是否存在
func NewService(repo MessageStore, accountRepo AccountLookup) *Service {
	return &Service{repo: repo, accountRepo: accountRepo}
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// Repository.Send 是私信落库的最后一跳，也是空内容校验的第二道防线
// （handler 已在绑定后按 to_id/content 拒绝过一次）。
//
// 这里返回带码错误而不是裸 errors.New：裸错误会被分类器判成 500，
// 而"内容为空"是纯粹的客户端问题，应为 400。文案逐字未变，
// 调用方看到的仍是 "content is required"。
func (r *Repository) Send(ctx context.Context, m *Message) error {
	m.Content = strings.TrimSpace(m.Content)
	if m.Content == "" {
		return apierror.BadRequest("content is required")
	}
	m.CreatedAt = time.Now()
	return r.db.WithContext(ctx).Create(m).Error
}

func (r *Repository) List(ctx context.Context, userID, peerID uint, limit int) ([]Message, error) {
	var msgs []Message
	err := r.db.WithContext(ctx).
		Where("(from_id = ? AND to_id = ?) OR (from_id = ? AND to_id = ?)", userID, peerID, peerID, userID).
		Order("created_at desc").
		Limit(limit).
		Find(&msgs).Error
	return msgs, err
}

func (h *Handler) Send(c *gin.Context) {
	fromID, err := jwt.GetAccountID(c)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	var req SendRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierror.RespondBinding(c, err)
		return
	}
	if req.ToID == 0 || strings.TrimSpace(req.Content) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "to_id and content are required"})
		return
	}
	// 校验接收者存在：避免写入永远无法投递的孤儿私信
	if h.service.accountRepo != nil {
		if _, err := h.service.accountRepo.FindByID(c.Request.Context(), req.ToID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "recipient not found"})
			return
		}
	}
	m := &Message{FromID: fromID, ToID: req.ToID, Content: req.Content}
	if err := h.service.repo.Send(c.Request.Context(), m); err != nil {
		apierror.Respond(c, err)
		return
	}
	c.JSON(http.StatusOK, m)
}

func (h *Handler) List(c *gin.Context) {
	userID, err := jwt.GetAccountID(c)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	var req ListRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierror.RespondBinding(c, err)
		return
	}
	if req.PeerID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "peer_id is required"})
		return
	}
	msgs, err := h.service.repo.List(c.Request.Context(), userID, req.PeerID, 50)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	if msgs == nil {
		msgs = []Message{}
	}
	c.JSON(http.StatusOK, ListResponse{Messages: msgs})
}
