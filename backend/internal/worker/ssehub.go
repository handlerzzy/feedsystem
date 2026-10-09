package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/handlerzzy/feedsystem/internal/apierror"
	"github.com/handlerzzy/feedsystem/internal/auth"
	"github.com/handlerzzy/feedsystem/internal/logging"
	jwt "github.com/handlerzzy/feedsystem/internal/middleware/jwt"
	rediscache "github.com/handlerzzy/feedsystem/internal/middleware/redis"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

type SSEHub struct {
	mu      sync.RWMutex
	clients map[uint][]chan *Notification
	db      *gorm.DB

	// cache 为空表示不启用跨副本扇出：推送只投给本进程的连接。
	// 单副本部署下这完全够用；多副本时才会丢推送（见 Push 的说明）。
	cache *rediscache.Client
	// instanceID 用来识别"这条广播是我自己发的"，避免同一条通知被投递两次。
	instanceID string
}

func NewSSEHub(db *gorm.DB) *SSEHub {
	return &SSEHub{
		clients:    make(map[uint][]chan *Notification),
		db:         db,
		instanceID: newInstanceID(),
	}
}

// WithFanout 启用跨副本扇出：推送经 Redis Pub/Sub 广播，每个副本各自投给自己
// 持有的连接。返回 h 本身以便链式调用（cmd/main.go 里是单行构造）。
func (h *SSEHub) WithFanout(cache *rediscache.Client) *SSEHub {
	h.cache = cache
	return h
}

// fanoutChannel 是本项目专用的广播频道。用 cache.Key 加统一前缀，
// 避免与共用同一个 Redis 的其它环境串台。
func (h *SSEHub) fanoutChannel() string {
	return h.cache.Key("sse:notify")
}

// fanoutMessage 是广播体。
//
// origin 不是可选的：Redis Pub/Sub 会把消息投给**包括发布者在内**的所有订阅者，
// 没有它就等于本进程会收到自己刚投递过的通知，用户看到两条。
type fanoutMessage struct {
	Origin       string        `json:"origin"`
	UserID       uint          `json:"user_id"`
	Notification *Notification `json:"notification"`
}

// Push 把通知推给目标用户。
//
// 多副本下这里曾经是错的：notification 消费者（三个队列）在所有 API 副本之间是
// **竞争消费**，同一条 MQ 消息只会落到其中一个副本上，而 SSE 连接是**每个副本各自
// 持有**的。于是用户连在哪个副本上，决定了这次推送能不能到他手里——连在另一个
// 副本上就什么都收不到，只能等客户端轮询 /notification/list。
//
// 现在：先投本进程（保证单副本行为与 Redis 无关，Redis 挂了推送照样能用），
// 再广播给其它副本（让持有该用户连接的副本也能投一次）。
func (h *SSEHub) Push(userID uint, n *Notification) {
	if h == nil || n == nil {
		return
	}
	h.deliverLocal(userID, n)

	if h.cache == nil {
		return
	}
	payload, err := json.Marshal(fanoutMessage{Origin: h.instanceID, UserID: userID, Notification: n})
	if err != nil {
		logging.L().Warn("SSE 广播序列化失败，仅本副本收到推送", zap.Error(err))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), rediscache.PublishTimeout)
	defer cancel()
	if err := h.cache.Publish(ctx, h.fanoutChannel(), payload); err != nil {
		// 推送不是可靠投递：通知已经落库，客户端轮询仍能拿到，故为 Warn。
		logging.L().Warn("SSE 广播失败，其它副本收不到本次推送", zap.Error(err))
	}
}

// deliverLocal 把通知投给**本进程**持有的连接。不关心副本，不做广播。
func (h *SSEHub) deliverLocal(userID uint, n *Notification) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	chs, ok := h.clients[userID]
	if !ok {
		return
	}
	for _, ch := range chs {
		select {
		case ch <- n:
		default:
			// 客户端消费不过来（连接慢或已死）。这里是尽力而为：丢掉这一条，
			// 而不是阻塞整个推送路径。客户端重连后会走 /notification/list 补齐。
		}
	}
}

// StartFanout 阻塞运行广播订阅循环，直到 ctx 被取消。
//
// 未启用扇出（cache 为空）时直接返回——调用方无需分支。
func (h *SSEHub) StartFanout(ctx context.Context) {
	if h == nil || h.cache == nil {
		logging.Ctx(ctx).Info("SSE 跨副本扇出未启用（无 redis），推送只投本进程")
		return
	}

	logging.Ctx(ctx).Info("SSE 跨副本扇出已启用", zap.String("channel", h.fanoutChannel()))
	err := h.cache.SubscribeLoop(ctx, h.fanoutChannel(), func(payload []byte) {
		var msg fanoutMessage
		if err := json.Unmarshal(payload, &msg); err != nil {
			// 频道里出现无法解析的内容：可能是版本不一致或别的程序写进来的。
			// 丢弃这一条并继续，不要因为一条坏消息终止整个订阅。
			logging.Ctx(ctx).Warn("SSE 广播消息解析失败，已丢弃", zap.Error(err))
			return
		}
		if msg.Origin == h.instanceID {
			return // 本进程发出的，Push 里已经投过了
		}
		if msg.UserID == 0 || msg.Notification == nil {
			return
		}
		h.deliverLocal(msg.UserID, msg.Notification)
	})
	if err != nil && ctx.Err() == nil {
		// 订阅中断即所有副本间的推送都断了（本进程内推送仍可用），故为 Error。
		logging.Ctx(ctx).Error("SSE 广播订阅中断", zap.Error(err))
	}
}

// newInstanceID 生成一个进程内唯一的标识，用于识别自己发出的广播。
// 用随机值而不是 PID：不同主机上的副本 PID 可能相同。
func newInstanceID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// 极不可能发生；退化成时间戳，仍能区分不同进程。
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func (h *SSEHub) Subscribe(userID uint) chan *Notification {
	ch := make(chan *Notification, 20)
	h.mu.Lock()
	h.clients[userID] = append(h.clients[userID], ch)
	h.mu.Unlock()
	return ch
}

func (h *SSEHub) Unsubscribe(userID uint, ch chan *Notification) {
	h.mu.Lock()
	defer h.mu.Unlock()
	chs := h.clients[userID]
	for i, c := range chs {
		if c == ch {
			chs = append(chs[:i], chs[i+1:]...)
			if len(chs) == 0 {
				delete(h.clients, userID)
			} else {
				h.clients[userID] = chs
			}
			close(c)
			return
		}
	}
}

func sseAccountID(c *gin.Context) (uint, bool) {
	accountID, ok := c.Get("accountID")
	if !ok {
		return 0, false
	}
	userID, ok := accountID.(uint)
	return userID, ok && userID != 0
}

// SSERequireAuth 校验 SSE/通知接口的 token。
// 与 jwt.JWTAuth 共用 CheckAndBind，因此 logout / 改名 / 改密后
// 已吊销的 token 在这里同样会被拒绝（而不是只做 ParseToken）。
//
// 参数取 jwt.AccountLookup 而不是 *account.AccountRepository：这条路径正是
// 第一批修的"SSE 鉴权旁路"缺陷所在（原先只验签名、不验吊销），抽成接口后
// 它才能被单测覆盖。*account.AccountRepository 隐式满足该接口，调用方无需改动。
func (h *SSEHub) SSERequireAuth(accountRepo jwt.AccountLookup, cache *rediscache.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 1) 优先 Authorization: Bearer
		token := ""
		if authHeader := c.GetHeader("Authorization"); authHeader != "" {
			token = authHeader
			if len(token) > 7 && token[:7] == "Bearer " {
				token = token[7:]
			}
		}
		// 2) 回退 ?token= —— EventSource 无法自定义 header，README 已文档化该用法
		if token == "" {
			token = c.Query("token")
		}
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
			return
		}
		claims, err := auth.ParseToken(token)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			return
		}
		// 3) 与 JWTAuth 一致的吊销校验（Redis 比对 + DB 兜底）
		if err := jwt.CheckAndBind(c, claims, token, accountRepo, cache); err != nil {
			apierror.Respond(c, err)
			c.Abort()
			return
		}
		c.Next()
	}
}

func (h *SSEHub) SSEHandler(c *gin.Context) {
	userID, ok := sseAccountID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid account"})
		return
	}

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.WriteHeader(http.StatusOK)

	ch := h.Subscribe(userID)
	defer h.Unsubscribe(userID, ch)

	ctx := c.Request.Context()

	// 用 ResponseController 给每次写设置 deadline。
	// 目的：客户端半开连接（拔网线/休眠）时，Write 不会永久阻塞。
	// 没有这个保护，goroutine + 连接 + channel 会一起泄漏，
	// 因为 defer Unsubscribe 永远不会执行。
	rc := http.NewResponseController(c.Writer)

	// writeEvent 写一次 SSE 数据并强制 flush。返回 error 表示连接已不可用。
	writeEvent := func(payload string) error {
		// 每次写之前重设 deadline（ResponseController 的 deadline 是绝对时间，需重设）
		if err := rc.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(c.Writer, "%s", payload); err != nil {
			return err
		}
		return rc.Flush()
	}

	for {
		select {
		case <-ctx.Done():
			return
		case n, ok := <-ch:
			if !ok {
				return
			}
			b, _ := json.Marshal(n)
			if err := writeEvent(fmt.Sprintf("data: %s\n\n", b)); err != nil {
				return // 关键：写失败立即退出，触发 defer Unsubscribe
			}
		case <-time.After(30 * time.Second):
			// SSE 注释行做心跳，防止中间代理按空闲超时掐断连接
			if err := writeEvent(": keepalive\n\n"); err != nil {
				return
			}
		}
	}
}

func (h *SSEHub) ListHandler(c *gin.Context) {
	userID, ok := sseAccountID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid account"})
		return
	}

	var notifications []Notification
	if err := h.db.WithContext(c.Request.Context()).
		Where("recipient_id = ?", userID).
		Order("created_at desc").
		Limit(50).
		Find(&notifications).Error; err != nil {
		apierror.Respond(c, err)
		return
	}
	if notifications == nil {
		notifications = []Notification{}
	}
	c.JSON(200, gin.H{"notifications": notifications})
}

func (h *SSEHub) MarkReadHandler(c *gin.Context) {
	userID, ok := sseAccountID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid account"})
		return
	}

	var req struct {
		ID *uint `json:"id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		apierror.RespondBinding(c, err)
		return
	}

	var err error
	if req.ID != nil {
		err = h.db.WithContext(c.Request.Context()).Model(&Notification{}).Where("id = ? AND recipient_id = ?", *req.ID, userID).Update("is_read", true).Error
	} else {
		err = h.db.WithContext(c.Request.Context()).Model(&Notification{}).Where("recipient_id = ?", userID).Update("is_read", true).Error
	}
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	c.JSON(200, gin.H{"message": "ok"})
}

func (h *SSEHub) UnreadCountHandler(c *gin.Context) {
	userID, ok := sseAccountID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid account"})
		return
	}

	var count int64
	if err := h.db.WithContext(c.Request.Context()).Model(&Notification{}).Where("recipient_id = ? AND is_read = ?", userID, false).Count(&count).Error; err != nil {
		apierror.Respond(c, err)
		return
	}
	c.JSON(200, gin.H{"count": count})
}

func (h *SSEHub) RegisterRoutes(r *gin.Engine, group *gin.RouterGroup) {
	group.GET("/stream", h.SSEHandler)
	group.POST("/list", h.ListHandler)
	group.POST("/markRead", h.MarkReadHandler)
	group.POST("/unreadCount", h.UnreadCountHandler)
}

var _ NotificationHub = (*SSEHub)(nil)
