package video

import "time"

type Comment struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Username  string    `json:"username"`
	VideoID   uint      `gorm:"index:idx_comment_video_created,priority:1" json:"video_id"`
	AuthorID  uint      `json:"author_id"`
	Content   string    `gorm:"type:text" json:"content"`
	CreatedAt time.Time `gorm:"autoCreateTime;index:idx_comment_video_created,priority:2" json:"created_at"`
}

type PublishCommentRequest struct {
	VideoID uint   `json:"video_id"`
	Content string `json:"content"`
}

type DeleteCommentRequest struct {
	CommentID uint `json:"comment_id"`
}

type GetAllCommentsRequest struct {
	VideoID uint `json:"video_id"`
}
