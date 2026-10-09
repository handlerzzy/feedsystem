package social

import "github.com/handlerzzy/feedsystem/internal/account"

type Social struct {
	ID         uint `gorm:"primaryKey"`
	FollowerID uint `gorm:"not null;index:idx_social_follower;index:idx_social_vlogger_follower,priority:2;uniqueIndex:idx_social_follower_vlogger"`
	VloggerID  uint `gorm:"not null;index:idx_social_vlogger;index:idx_social_vlogger_follower,priority:1;uniqueIndex:idx_social_follower_vlogger"`
}

type FollowRequest struct {
	VloggerID uint `json:"vlogger_id"`
}

type UnfollowRequest struct {
	VloggerID uint `json:"vlogger_id"`
}

type GetAllFollowersRequest struct {
	VloggerID uint `json:"vlogger_id"`
}

type GetAllFollowersResponse struct {
	Followers     []*account.PublicAccount `json:"followers"`
	FollowerCount int64                    `json:"follower_count"`
}

type GetAllVloggersResponse struct {
	Vloggers     []*account.PublicAccount `json:"vloggers"`
	VloggerCount int64                    `json:"vlogger_count"`
}

type SocialCounts struct {
	FollowerCount int64 `json:"follower_count"`
	VloggerCount  int64 `json:"vlogger_count"`
}

type GetAllVloggersRequest struct {
	FollowerID uint `json:"follower_id"`
}
