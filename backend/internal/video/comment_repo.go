package video

import (
	"context"
	"errors"

	"github.com/handlerzzy/feedsystem/internal/apierror"

	"gorm.io/gorm"
)

type CommentRepository struct {
	db *gorm.DB
}

func NewCommentRepository(db *gorm.DB) *CommentRepository {
	return &CommentRepository{db: db}
}

// MentionNotification 对应 notifications 表里由「@提及」产生的一行。
// 此前它是 notifyMentions 里的匿名 struct，随事务下沉一并具名化。
type MentionNotification struct {
	RecipientID uint
	SenderID    uint
	Type        string
	TargetID    uint
	Content     string
}

// PublishWithPopularity 在单个事务内写入评论并把视频热度 +1。
//
// 之所以放在 repo：该事务原先写在 CommentService 里、直接访问
// CommentRepository 的私有 db 字段，service 层因此无法被单测覆盖。
func (r *CommentRepository) PublishWithPopularity(ctx context.Context, comment *Comment) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Select("id").First(&Video{}, comment.VideoID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return apierror.NotFound("video not found")
			}
			return err
		}
		if err := tx.Create(comment).Error; err != nil {
			return err
		}
		return tx.Model(&Video{}).Where("id = ?", comment.VideoID).
			UpdateColumn("popularity", gorm.Expr("popularity + 1")).Error
	})
}

// AccountIDByUsername 按用户名查账号 ID，供 @提及 解析收件人。
//
// 保持原有语义：查不到时返回 (0, nil)，由调用方用 accID == 0 判断，
// 不把"用户不存在"当成错误（提及一个不存在的用户名是正常情况）。
func (r *CommentRepository) AccountIDByUsername(ctx context.Context, username string) (uint, error) {
	var accID uint
	err := r.db.WithContext(ctx).Table("accounts").
		Where("username = ?", username).Select("id").Scan(&accID).Error
	return accID, err
}

// CreateMentionNotification 写入一条 @提及 通知。
func (r *CommentRepository) CreateMentionNotification(ctx context.Context, n *MentionNotification) error {
	return r.db.WithContext(ctx).Table("notifications").Create(n).Error
}

func (r *CommentRepository) CreateComment(ctx context.Context, comment *Comment) error {
	return r.db.WithContext(ctx).Create(comment).Error
}

func (r *CommentRepository) DeleteComment(ctx context.Context, comment *Comment) error {
	return r.db.WithContext(ctx).Delete(comment).Error
}

func (r *CommentRepository) GetAllComments(ctx context.Context, videoID uint) ([]Comment, error) {
	var comments []Comment
	err := r.db.WithContext(ctx).
		Where("video_id = ?", videoID).
		Order("created_at asc").
		Limit(200).
		Find(&comments).Error
	return comments, err
}

func (r *CommentRepository) IsExist(ctx context.Context, id uint) (bool, error) {
	var comment Comment
	if err := r.db.WithContext(ctx).First(&comment, id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (r *CommentRepository) GetByID(ctx context.Context, id uint) (*Comment, error) {
	var comment Comment
	if err := r.db.WithContext(ctx).First(&comment, id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &comment, nil
}
