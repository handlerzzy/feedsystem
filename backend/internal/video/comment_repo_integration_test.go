package video_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/handlerzzy/feedsystem/internal/apierror"
	"github.com/handlerzzy/feedsystem/internal/video"
)

// TestPublishWithPopularityMissingVideoIntegration 覆盖 comment_repo.go 里
// "回退事务内视频被删"的竞态分支：
//
//	if errors.Is(err, gorm.ErrRecordNotFound) {
//		return errors.New("video not found")   // 旧：分类器不认识 => 500
//	}
//
// 现在返回 apierror.NotFound("video not found")：状态码 404、文案逐字不变。
//
// 必须用真实 MySQL：该分支在 GORM 事务回调内部，只有真库才会产生
// ErrRecordNotFound。未设置 TEST_MYSQL_DSN 时跳过（openIntegrationDB 定义在
// like_repo_integration_test.go）。
func TestPublishWithPopularityMissingVideoIntegration(t *testing.T) {
	db := openIntegrationDB(t)
	repo := video.NewCommentRepository(db)

	comment := &video.Comment{
		VideoID:   999999999, // 必然不存在
		AuthorID:  900006,
		Username:  "itest-user",
		Content:   "itest comment",
		CreatedAt: time.Now(),
	}

	err := repo.PublishWithPopularity(context.Background(), comment)
	if err == nil {
		t.Fatal("视频不存在时必须报错，实际 nil（会给一个不存在的视频写评论）")
	}
	if got := apierror.ClassifyHTTPStatus(err); got != http.StatusNotFound {
		t.Errorf("HTTP status = %d, want 404（视频不存在是 404，不是 500）", got)
	}
	if got := err.Error(); got != "video not found" {
		t.Errorf("err.Error() = %q, want %q（对外文案不得改变）", got, "video not found")
	}
	// 事务必须回滚：不得留下评论行。
	var n int64
	db.Model(&video.Comment{}).Where("video_id = ?", comment.VideoID).Count(&n)
	if n != 0 {
		t.Errorf("视频不存在时不应写入评论，实际有 %d 行", n)
	}
}
