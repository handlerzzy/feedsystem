package video

import (
	"context"
	"errors"

	"github.com/handlerzzy/feedsystem/internal/apierror"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

type LikeRepository struct {
	db *gorm.DB
}

func NewLikeRepository(db *gorm.DB) *LikeRepository {
	return &LikeRepository{db: db}
}

func isDupKey(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

// errSkipCountUpdate 是事务内部的控制流哨兵：并发下目标状态已达成
// （点赞行已存在 / 已被删除）时，视为幂等成功，但必须跳过计数更新。
// 用非 nil error 是为了让事务回滚——此时本来就没有任何写入需要提交。
//
// 它只在 repo 内部流转：对调用方表现为 (false, nil)，而不是错误。
var errSkipCountUpdate = errors.New("skip count update")

// LikeWithCounts 在单个事务内插入点赞行，并自增 likes_count / popularity。
//
// 返回值 created 表示"本次真的插入了新行"：
//   - (true, nil)  插入成功，计数已自增
//   - (false, nil) 点赞行已存在（并发或客户端重试）——幂等成功，计数未变
//   - (false, err) 其他错误；视频不存在时 err 为 apierror.ErrNotFound
//
// 之所以把事务放在 repo 而不是 service：service 层不应持有 *gorm.DB。
// 此前该事务写在 LikeService 里，直接访问 LikeRepository 的私有 db 字段，
// 导致这段逻辑无法被 mock、也就无法单测。
func (r *LikeRepository) LikeWithCounts(ctx context.Context, like *Like) (created bool, err error) {
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Select("id").First(&Video{}, like.VideoID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return apierror.ErrNotFound
			}
			return err
		}
		if err := tx.Create(like).Error; err != nil {
			if isDupKey(err) {
				// 并发/重试下该点赞行已存在：目标状态已达成，视为幂等成功。
				// 用哨兵让事务回滚，跳过下面的计数自增（插入失败就不能加计数）。
				return errSkipCountUpdate
			}
			return err
		}
		if err := tx.Model(&Video{}).Where("id = ?", like.VideoID).
			UpdateColumn("likes_count", gorm.Expr("likes_count + 1")).Error; err != nil {
			return err
		}
		return tx.Model(&Video{}).Where("id = ?", like.VideoID).
			UpdateColumn("popularity", gorm.Expr("popularity + 1")).Error
	})
	if errors.Is(err, errSkipCountUpdate) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// UnlikeWithCounts 在单个事务内删除点赞行，并自减 likes_count / popularity。
//
// 返回值 deleted 表示"本次真的删掉了行"：
//   - (true, nil)  删除成功，计数已自减
//   - (false, nil) 点赞行本就不存在——幂等成功，计数未变
//   - (false, err) 其他错误
//
// 计数使用 GREATEST(x - 1, 0) 兜底，避免并发下减成负数。
func (r *LikeRepository) UnlikeWithCounts(ctx context.Context, videoID, accountID uint) (deleted bool, err error) {
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		del := tx.Where("video_id = ? AND account_id = ?", videoID, accountID).Delete(&Like{})
		if del.Error != nil {
			return del.Error
		}
		if del.RowsAffected == 0 {
			// 并发下点赞行已被别的请求删掉：目标状态已达成，视为幂等成功。
			// 用哨兵让事务回滚，跳过下面的计数自减（没有删到行就不能减计数）。
			return errSkipCountUpdate
		}
		if err := tx.Model(&Video{}).Where("id = ?", videoID).
			UpdateColumn("likes_count", gorm.Expr("GREATEST(likes_count - 1, 0)")).Error; err != nil {
			return err
		}
		return tx.Model(&Video{}).Where("id = ?", videoID).
			UpdateColumn("popularity", gorm.Expr("GREATEST(popularity - 1, 0)")).Error
	})
	if errors.Is(err, errSkipCountUpdate) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (r *LikeRepository) Like(ctx context.Context, like *Like) error {
	return r.db.WithContext(ctx).Create(like).Error
}

func (r *LikeRepository) Unlike(ctx context.Context, like *Like) error {
	return r.db.WithContext(ctx).
		Where("video_id = ? AND account_id = ?", like.VideoID, like.AccountID).
		Delete(&Like{}).Error
}

func (r *LikeRepository) LikeIgnoreDuplicate(ctx context.Context, like *Like) (created bool, err error) {
	if like == nil || like.VideoID == 0 || like.AccountID == 0 {
		return false, nil
	}
	err = r.db.WithContext(ctx).Create(like).Error
	if err == nil {
		return true, nil
	}
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
		return false, nil
	}
	return false, err
}

func (r *LikeRepository) DeleteByVideoAndAccount(ctx context.Context, videoID, accountID uint) (deleted bool, err error) {
	if videoID == 0 || accountID == 0 {
		return false, nil
	}
	res := r.db.WithContext(ctx).
		Where("video_id = ? AND account_id = ?", videoID, accountID).
		Delete(&Like{})
	return res.RowsAffected > 0, res.Error
}

func (r *LikeRepository) IsLiked(ctx context.Context, videoID, accountID uint) (bool, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&Like{}).
		Where("video_id = ? AND account_id = ?", videoID, accountID).
		Count(&count).Error
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func (r *LikeRepository) BatchGetLiked(ctx context.Context, videoIDs []uint, accountID uint) (map[uint]bool, error) {
	likeMap := make(map[uint]bool)
	if len(videoIDs) == 0 {
		return likeMap, nil
	}
	if accountID == 0 {
		return likeMap, nil
	}
	var likes []Like
	err := r.db.WithContext(ctx).Model(&Like{}).
		Where("video_id IN ? AND account_id = ?", videoIDs, accountID).
		Find(&likes).Error
	if err != nil {
		return nil, err
	}
	for _, like := range likes {
		likeMap[like.VideoID] = true
	}
	return likeMap, nil
}

func (r *LikeRepository) ListLikedVideos(ctx context.Context, accountID uint) ([]Video, error) {
	var videos []Video
	if accountID == 0 {
		return videos, nil
	}
	err := r.db.WithContext(ctx).
		Model(&Video{}).
		Joins("JOIN likes ON likes.video_id = videos.id").
		Where("likes.account_id = ?", accountID).
		Order("likes.created_at desc").
		Limit(200).
		Find(&videos).Error
	if err != nil {
		return nil, err
	}
	return videos, nil
}
