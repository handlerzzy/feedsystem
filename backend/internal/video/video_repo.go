package video

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type VideoRepository struct {
	db *gorm.DB
}

func NewVideoRepository(db *gorm.DB) *VideoRepository {
	return &VideoRepository{db: db}
}

// PublishWithOutbox 在单个事务内写入视频、本地消息表（outbox）记录与标签。
//
// 三者必须同成败：outbox 行是时间线异步投递的唯一依据，若视频写成功而
// outbox 写失败，该视频将永远不会进入任何人的时间线；反之则会产生指向
// 不存在视频的消息。此前该事务写在 VideoService 里、直接访问
// VideoRepository 的私有 db 字段，service 层因此无法被单测覆盖。
func (vr *VideoRepository) PublishWithOutbox(ctx context.Context, video *Video) error {
	return vr.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(video).Error; err != nil {
			return err
		}

		msg := OutboxMsg{
			VideoID:    video.ID,
			EventType:  "video_published",
			Status:     "pending",
			CreateTime: video.CreateTime,
		}
		if err := tx.Create(&msg).Error; err != nil {
			return err
		}

		tags := ExtractTags(video.Title + " " + video.Description)
		for _, tagName := range tags {
			tagID, err := resolveTagID(tx, tagName)
			if err != nil {
				return err
			}
			if err := tx.Create(&VideoTag{VideoID: video.ID, TagID: tagID}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// resolveTagID 返回标签的 ID，不存在则创建。
//
// 这里刻意**不用** GORM 的 FirstOrCreate。它的实现是"先 SELECT，查不到再
// INSERT"，两步之间没有任何保护：
//
//	tx.Where("name = ?", name).FirstOrCreate(&tag, Tag{Name: name})
//
// 两个并发事务同时发布同一个**全新**话题时会双双查不到、然后一起去 INSERT，
// 由 tags.idx_tags_name 这个唯一键决定谁活下来，其余全部撞 1062。
// 该错误会冒泡出事务并让整个发布回滚——用户提交的是完全合法的视频，失败却
// 与他们的输入毫无关系，且对外只表现为一个 500。
//
// 用真库实测（8 个 goroutine 经 barrier 同时发布同一个新话题，重复 5 次）：
//
//	改用本函数前：7/8、7/8、7/8、7/8、7/8 失败
//	改用本函数后：见 video_repo_integration_test.go 的并发用例
//
// 竞态窗口并不只有"同时按下发布"那么窄：SELECT 与 INSERT 之间还夹着视频行
// 和 outbox 行的两次写入往返，所以几百毫秒内发布同一新话题的人都会互相踩。
//
// 修法是把它压成单条 upsert，让数据库的唯一键来仲裁先后，而不是让应用先查
// 后写。
//
// 关于冲突子句的选型（下面这条 SQL 用 GORM 的 DryRun 实测得到）：
//
//	本函数选用的写法 => INSERT INTO `tags` (`name`) VALUES (?) ON DUPLICATE KEY UPDATE `name`=name
//	clause.OnConflict{DoNothing: true} => INSERT INTO `tags` (`name`) VALUES (?) ON DUPLICATE KEY UPDATE `id`=`id`
//
// 两者在语义上是等价的（都是无副作用的空更新），实测也都保留数据类错误。
// 这里仍选前者，是为了不依赖 GORM 对 DoNothing 的翻译：万一将来它改成
// INSERT IGNORE，所有错误都会被降级成 warning，"标签名超长"这类真实的数据
// 错误会被静默截断写进库，而显式的 DoUpdates 写法不可能退化到那一步。
func resolveTagID(tx *gorm.DB, name string) (uint, error) {
	tag := Tag{Name: name}
	if err := tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "name"}},
		DoUpdates: clause.Assignments(map[string]any{"name": gorm.Expr("name")}),
	}).Create(&tag).Error; err != nil {
		return 0, err
	}
	if tag.ID != 0 {
		return tag.ID, nil
	}

	// 走到这里说明命中了已存在的行：ON DUPLICATE KEY UPDATE 在"没有真正改变
	// 任何值"时不会回填自增 ID（name=name 正是这种情况），所以还得查一次。
	//
	// 这次查询用了当前读（FOR UPDATE）而不是普通 SELECT，但**它今天并不是
	// 必需的**，这点实测过：换成普通 SELECT 后并发用例连跑 20 轮全绿。
	// 原因是在本函数之前，整个事务里只有写操作（视频行、outbox 行、上面那条
	// upsert），没有任何一致性读，所以这个 SELECT 恰好是本事务的第一次一致性
	// 读，会当场建立新的 read view，看得见刚提交的那一行。
	//
	// 那为什么还留着 FOR UPDATE：这个"恰好"太脆弱。REPEATABLE READ 下 read
	// view 在事务第一次一致性读时固定，只要以后有人在本函数之前往这个事务里
	// 加一条普通 SELECT（比如标题查重），上面的推理立刻失效——回查会读到
	// "这行还不存在"的旧快照、返回 ErrRecordNotFound，并发发布又变回集体失败，
	// 而且极难复现。加锁读永远读最新已提交版本，不受影响；至于代价，上面那条
	// upsert 已经把这行 X 锁住了，所以这个 FOR UPDATE 不带来额外阻塞。
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("name = ?", name).First(&tag).Error; err != nil {
		return 0, err
	}
	return tag.ID, nil
}

func (vr *VideoRepository) CreateVideo(ctx context.Context, video *Video) error {
	if err := vr.db.WithContext(ctx).Create(video).Error; err != nil {
		return err
	}
	return nil
}

func (vr *VideoRepository) CreateMsg(ctx context.Context, Msg *OutboxMsg) error {
	if err := vr.db.WithContext(ctx).Create(Msg).Error; err != nil {
		return err
	}
	return nil
}

func (vr *VideoRepository) DeleteVideo(ctx context.Context, id uint) error {
	if err := vr.db.WithContext(ctx).Delete(&Video{}, id).Error; err != nil {
		return err
	}
	return nil
}

func (vr *VideoRepository) ListByAuthorID(ctx context.Context, authorID int64) ([]Video, error) {
	var videos []Video
	if err := vr.db.WithContext(ctx).
		Where("author_id = ?", authorID).
		Order("create_time desc").
		Limit(200).
		Find(&videos).Error; err != nil {
		return nil, err
	}
	return videos, nil
}

func (vr *VideoRepository) GetByID(ctx context.Context, id uint) (*Video, error) {
	var video Video
	if err := vr.db.WithContext(ctx).First(&video, id).Error; err != nil {
		return (*Video)(nil), err
	}
	return &video, nil
}

func (vr *VideoRepository) IsExist(ctx context.Context, id uint) (bool, error) {
	var video Video
	if err := vr.db.WithContext(ctx).First(&video, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (vr *VideoRepository) ChangeLikesCount(ctx context.Context, id uint, change int64) error {
	if err := vr.db.WithContext(ctx).Model(&Video{}).
		Where("id = ?", id).
		UpdateColumn("likes_count", gorm.Expr("GREATEST(likes_count + ?, 0)", change)).Error; err != nil {
		return err
	}
	return nil
}

func (vr *VideoRepository) ChangePopularity(ctx context.Context, id uint, change int64) error {
	if err := vr.db.WithContext(ctx).Model(&Video{}).
		Where("id = ?", id).
		UpdateColumn("popularity", gorm.Expr("GREATEST(popularity + ?, 0)", change)).Error; err != nil {
		return err
	}
	return nil
}

func (vr *VideoRepository) CountByAuthor(ctx context.Context, authorID uint) (int64, error) {
	var count int64
	if err := vr.db.WithContext(ctx).Model(&Video{}).Where("author_id = ?", authorID).Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

func (vr *VideoRepository) TotalLikesByAuthor(ctx context.Context, authorID uint) (int64, error) {
	var total int64
	if err := vr.db.WithContext(ctx).Model(&Video{}).Where("author_id = ?", authorID).Select("COALESCE(SUM(likes_count), 0)").Scan(&total).Error; err != nil {
		return 0, err
	}
	return total, nil
}
