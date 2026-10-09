// Command evalbaseline 导出离线评测集并计算**现有排序的基线指标**。
//
// 用法（在 backend/ 下执行）：
//
//	# 1) 从 MySQL 导出快照并评测（推荐的第一次用法）
//	go run ./cmd/evalbaseline \
//	  -dsn 'root:123456@tcp(127.0.0.1:3307)/feedflow?charset=utf8mb4&parseTime=True&loc=Local' \
//	  -snapshot-out testdata/eval/snapshot.json \
//	  -json testdata/eval/baseline.json \
//	  -markdown testdata/eval/baseline.md
//
//	# 2) 用已落盘的快照复现同一组数字（不需要数据库）
//	go run ./cmd/evalbaseline -snapshot testdata/eval/snapshot.json
//
// 为什么既支持导出又支持复现：基线数字的意义在于**可复现**。库里数据每天在变，
// 半年后要回答"当时基线是多少、怎么算出来的"，只能靠那份冻结的快照。
//
// 这个命令不属于服务进程：它只被人工执行，不参与 API/Worker 的启动路径。
// 因此它可以直接读库、可以直接打印，不必遵守运行期的降级约定。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/handlerzzy/feedsystem/internal/config"
	"github.com/handlerzzy/feedsystem/internal/db"
	"github.com/handlerzzy/feedsystem/internal/evalset"
)

func main() {
	var (
		dsn         = flag.String("dsn", "", "MySQL DSN；留空则必须用 -snapshot 提供数据（也可用 MYSQL_DSN 环境变量）")
		snapshotIn  = flag.String("snapshot", "", "读取已导出的快照（JSON）而不是查库")
		snapshotOut = flag.String("snapshot-out", "", "把本次导出的快照写到该路径（推荐：让基线可复现）")
		jsonOut     = flag.String("json", "", "把评测结果写成 JSON")
		markdownOut = flag.String("markdown", "", "把评测结果写成 Markdown 摘要")
		splitRatio  = flag.Float64("split", 0.5, "时间切分比例：候选集取该分位之前发布的视频（显式传入时会重新切分并重算特征）")
		ksFlag      = flag.String("k", "5,10,20", "要计算的 K，逗号分隔")
	)
	flag.Parse()

	// 记录 -split 是否被显式传入：见上面 Rebuild 的注释。
	splitProvided = false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "split" {
			splitProvided = true
		}
	})

	ks, err := parseKs(*ksFlag)
	if err != nil {
		fatal(err)
	}

	snap, raw, err := loadSnapshot(*snapshotIn, *dsn, *splitRatio)
	if err != nil {
		fatal(err)
	}

	// 只有在用户**显式**传了 -split 时才按它重算切分点与特征。
	//
	// 为什么不总是重算：读一份冻结快照时，它的 split_at 就是"当时的口径"，
	// 重算会让历史数字变掉（那就不是"复现"了）。而显式传 -split 表达的是
	// "我要按新比例重切"，此时必须把 split_at 与特征一起重算——
	// 只改 split_ratio 字段而留着旧 split_at，报告就会写着 80% 实际按 50% 算。
	if splitProvided {
		if err := snap.Rebuild(*splitRatio, raw.videos, raw.likes, raw.comments); err != nil {
			fatal(err)
		}
	}

	if *snapshotOut != "" {
		if err := snap.Save(*snapshotOut); err != nil {
			fatal(fmt.Errorf("写快照失败: %w", err))
		}
		fmt.Printf("快照已写入 %s（videos=%d interactions=%d）\n",
			*snapshotOut, len(snap.Videos), len(snap.Interactions))
	}

	res, err := evalset.Evaluate(snap, evalset.BuiltinRankers(), ks)
	if err != nil {
		fatal(err)
	}
	res.SetProducer("backend/cmd/evalbaseline")

	fmt.Print(evalset.RenderText(res))

	if *jsonOut != "" {
		b, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			fatal(err)
		}
		if err := os.WriteFile(*jsonOut, append(b, '\n'), 0o644); err != nil {
			fatal(fmt.Errorf("写 JSON 失败: %w", err))
		}
		fmt.Printf("\n结果已写入 %s\n", *jsonOut)
	}
	if *markdownOut != "" {
		if err := os.WriteFile(*markdownOut, []byte(evalset.RenderMarkdown(res, snap)), 0o644); err != nil {
			fatal(fmt.Errorf("写 Markdown 失败: %w", err))
		}
		fmt.Printf("摘要已写入 %s\n", *markdownOut)
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "evalbaseline: %v\n", err)
	os.Exit(1)
}

// splitProvided 表示命令行是否显式指定了 -split（在 main 里赋值）。
var splitProvided bool

func parseKs(raw string) ([]int, error) {
	var ks []int
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// 用 strconv 而不是 Sscanf：Sscanf("%d") 会把 "5abc" 解析成 5，
		// 于是一个拼错的参数被静默接受（review 实测）。
		k, err := strconv.Atoi(part)
		if err != nil || k <= 0 {
			return nil, fmt.Errorf("-k 里出现非法值 %q（应为正整数，逗号分隔）", part)
		}
		ks = append(ks, k)
	}
	if len(ks) == 0 {
		return nil, fmt.Errorf("-k 不能为空")
	}
	return ks, nil
}

// rawData 是"未经特征派生"的原始数据。
//
// 之所以把"原始视频 + 互动"一路带回来而不是只返回快照：
// 用户显式要求重新切分时，必须能按新切分点重算特征。若只留派生后的快照，
// 重切就只能拿着旧特征凑合——那就又回到"特征含未来信息"的老问题上。
type rawData struct {
	videos   []evalset.Video
	likes    []evalset.Interaction
	comments []evalset.Interaction
}

func loadSnapshot(path, dsn string, splitRatio float64) (*evalset.Snapshot, rawData, error) {
	if path != "" {
		snap, err := evalset.Load(path)
		if err != nil {
			return nil, rawData{}, err
		}
		raw := splitInteractions(snap)
		return snap, raw, nil
	}
	if dsn == "" {
		dsn = os.Getenv("MYSQL_DSN")
	}
	if dsn == "" {
		return nil, rawData{}, fmt.Errorf("必须提供 -dsn（或用 MYSQL_DSN）或 -snapshot")
	}
	return exportFromMySQL(dsn, splitRatio)
}

// splitInteractions 把快照里的互动按类型拆开，供 ApplyFeatures/Rebuild 使用。
//
// 注意它拿到的是**已经派生过**的快照，但 ApplyFeatures 只依赖互动时间与
// 切分点，不依赖旧特征，因此重新切分仍然正确。
func splitInteractions(snap *evalset.Snapshot) rawData {
	raw := rawData{videos: snap.Videos}
	for _, it := range snap.Interactions {
		switch it.Kind {
		case evalset.KindLike:
			raw.likes = append(raw.likes, it)
		case evalset.KindComment:
			raw.comments = append(raw.comments, it)
		}
	}
	return raw
}

// interactionRow 是导出互动时用的行结构。
// 单独定义而不是复用 video.Like / video.Comment：导出只需要三列，
// 把整张实体拉进来会让"这个工具依赖哪些字段"变得不明确。
//
// AccountID 是 B1 新增的：没有它，导出出来的互动无法归属到用户，
// 按用户分组的评测就无从谈起（见 internal/evalset/peruser.go）。
type interactionRow struct {
	VideoID   uint      `gorm:"column:video_id"`
	AccountID uint      `gorm:"column:account_id"`
	At        time.Time `gorm:"column:at"`
}

// exportFromMySQL 读库导出候选视频与互动。
//
// 复用 internal/db.NewDB 与 internal/config.DatabaseConfig：连接串的拼装方式
// 必须与线上完全一致（字符集、时区、parseTime），否则时间字段的解析口径不同，
// 切分点与标签都会偏——而这种偏差在结果里完全看不出来。
func exportFromMySQL(dsn string, splitRatio float64) (*evalset.Snapshot, rawData, error) {
	cfg, err := configFromDSN(dsn)
	if err != nil {
		return nil, rawData{}, err
	}
	gdb, err := db.NewDB(cfg)
	if err != nil {
		return nil, rawData{}, fmt.Errorf("连接数据库失败: %w", err)
	}
	defer func() { _ = db.CloseDB(gdb) }()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var videos []evalset.Video
	if err := gdb.WithContext(ctx).
		Table("videos").
		Select("id, author_id, create_time, likes_count, popularity, title, description").
		Find(&videos).Error; err != nil {
		return nil, rawData{}, fmt.Errorf("导出 videos 失败: %w", err)
	}
	if len(videos) == 0 {
		return nil, rawData{}, fmt.Errorf("videos 表为空：没有可评测的数据")
	}

	// 点赞与评论分开查再合并：两张表的列名相同但语义不同（kind），
	// 而且 comments.created_at 允许为 NULL。用 UNION 会让 NULL 的排序语义
	// 依赖 MySQL 版本，不如在 Go 里显式合并来得清楚。
	//
	// 账号列名不同（likes.account_id / comments.author_id），这里统一别名成
	// account_id，让下面的合并逻辑只有一份。
	var likeRows []interactionRow
	if err := gdb.WithContext(ctx).
		Table("likes").
		Select("video_id, account_id AS account_id, created_at AS at").
		Where("created_at IS NOT NULL").
		Find(&likeRows).Error; err != nil {
		return nil, rawData{}, fmt.Errorf("导出 likes 失败: %w", err)
	}

	var commentRows []interactionRow
	if err := gdb.WithContext(ctx).
		Table("comments").
		Select("video_id, author_id AS account_id, created_at AS at").
		Where("created_at IS NOT NULL").
		Find(&commentRows).Error; err != nil {
		return nil, rawData{}, fmt.Errorf("导出 comments 失败: %w", err)
	}

	likes := make([]evalset.Interaction, 0, len(likeRows))
	for _, r := range likeRows {
		likes = append(likes, evalset.Interaction{
			VideoID: r.VideoID, AccountID: r.AccountID, Kind: evalset.KindLike, At: r.At,
		})
	}
	comments := make([]evalset.Interaction, 0, len(commentRows))
	for _, r := range commentRows {
		comments = append(comments, evalset.Interaction{
			VideoID: r.VideoID, AccountID: r.AccountID, Kind: evalset.KindComment, At: r.At,
		})
	}
	interactions := append(append([]evalset.Interaction{}, likes...), comments...)

	split, err := evalset.ComputeSplit(videos, splitRatio)
	if err != nil {
		return nil, rawData{}, err
	}

	// 关键：特征必须是**切分点之前**的状态。
	//
	// videos.likes_count / videos.popularity 是会被持续累加的列，导出时的值里
	// 已经包含了观测窗口内的互动——直接拿它当排序特征就等于"用答案排序"，
	// 基线数字会虚高（review 抓到的真实缺陷，实测候选视频的 likes_count
	// 恰好等于它之后获得的点赞数）。所以这里用互动行重算。
	features, note := evalset.AsOf(videos, likes, comments, split)

	return &evalset.Snapshot{
		GeneratedAt:  time.Now().UTC(),
		Source:       fmt.Sprintf("mysql/%s", cfg.DBName),
		SplitAt:      split,
		SplitRatio:   splitRatio,
		FeatureNote:  note,
		Videos:       features,
		Interactions: interactions,
	}, rawData{videos: videos, likes: likes, comments: comments}, nil
}

// configFromDSN 把一个 DSN 拆成 config.DatabaseConfig。
//
// 为什么让用户传 DSN 而不是分散的参数：运维手里通常已经有一条现成的 DSN
// （与 compose、与 .env 里的一致），复制粘贴比重新拼装更不容易出错。
func configFromDSN(dsn string) (config.DatabaseConfig, error) {
	at := strings.LastIndex(dsn, "@tcp(")
	if at < 0 {
		return config.DatabaseConfig{}, fmt.Errorf(
			"DSN 必须是 MySQL 形式（user:pass@tcp(host:port)/dbname），实际: %s", redactDSN(dsn))
	}
	userInfo := dsn[:at]
	rest := dsn[at+len("@tcp("):]
	closeParen := strings.Index(rest, ")")
	if closeParen < 0 {
		return config.DatabaseConfig{}, fmt.Errorf("DSN 缺少地址右括号: %s", redactDSN(dsn))
	}
	hostPort := rest[:closeParen]
	after := strings.TrimPrefix(rest[closeParen+1:], "/")
	dbName := after
	if idx := strings.Index(dbName, "?"); idx >= 0 {
		dbName = dbName[:idx]
	}
	if dbName == "" {
		return config.DatabaseConfig{}, fmt.Errorf("DSN 缺少库名: %s", redactDSN(dsn))
	}

	user := userInfo
	pass := ""
	if idx := strings.Index(userInfo, ":"); idx >= 0 {
		user = userInfo[:idx]
		pass = userInfo[idx+1:]
	}

	host := hostPort
	port := 3306
	if idx := strings.LastIndex(hostPort, ":"); idx >= 0 {
		host = hostPort[:idx]
		if _, err := fmt.Sscanf(hostPort[idx+1:], "%d", &port); err != nil {
			return config.DatabaseConfig{}, fmt.Errorf("DSN 端口非法: %s", redactDSN(dsn))
		}
	}
	if host == "" {
		return config.DatabaseConfig{}, fmt.Errorf("DSN 缺少主机名: %s", redactDSN(dsn))
	}

	return config.DatabaseConfig{
		Host:     host,
		Port:     port,
		User:     user,
		Password: pass,
		DBName:   dbName,
	}, nil
}

// redactDSN 去掉 DSN 里的密码，避免它出现在错误信息与日志里。
//
// 这不是洁癖：这个命令的报错经常被直接贴进 issue 或聊天窗口。
func redactDSN(dsn string) string {
	at := strings.LastIndex(dsn, "@tcp(")
	if at < 0 {
		return "[dsn]"
	}
	userInfo := dsn[:at]
	user := userInfo
	if idx := strings.Index(userInfo, ":"); idx >= 0 {
		user = userInfo[:idx]
	}
	return user + ":***" + dsn[at:]
}
