export type MessageResponse = { message: string }

export type DirectMessage = {
  id: number
  from_id: number
  to_id: number
  content: string
  is_read: boolean
  created_at: string
}

export type ListMessagesResponse = {
  messages: DirectMessage[]
}

export type TokenResponse = { token: string; refresh_token?: string; account_id?: number; username?: string }

export type Account = {
  id: number
  username: string
  avatar_url?: string
  bio?: string
}

export type Video = {
  id: number
  author_id: number
  username: string
  title: string
  description?: string
  play_url: string
  cover_url: string
  create_time: string
  likes_count: number
}

export type Comment = {
  id: number
  username: string
  video_id: number
  author_id: number
  content: string
  created_at: string
}

export type FeedAuthor = {
  id: number
  username: string
}

export type FeedVideoItem = {
  id: number
  author: FeedAuthor
  title: string
  description?: string
  play_url: string
  cover_url: string
  create_time: number
  likes_count: number
  is_liked: boolean
  // summary 是 P1 的 AI 摘要，reason 是 P2 的推荐理由。
  //
  // 两个字段都是**可选**的：服务端用 omitempty，AI 关闭或该视频没有
  // 可用信号时它们根本不出现在响应里。前端必须当"可能不存在"处理——
  // 这里是类型层面的提醒，模板里还有 v-if 兜底。
  //
  // 特别注意 reason：它必须来自真实信号（命中的兴趣标签、
  // 与点赞历史的语义相似、新内容探索）。**宁可空着也不要编**，
  // 因此 v-if 缺失时什么都不渲染，而不是显示"猜你喜欢"这类占位文案。
  summary?: string
  reason?: string
}

export type ListLatestResponse = {
  video_list: FeedVideoItem[]
  next_time: number
  has_more: boolean
}

export type ListLikesCountResponse = {
  video_list: FeedVideoItem[]
  next_likes_count_before?: number
  next_id_before?: number
  has_more: boolean
}

export type ListByPopularityResponse = {
  video_list: FeedVideoItem[]
  as_of: number
  next_offset: number
  has_more: boolean
  next_latest_popularity?: number
  next_latest_before?: string
  next_latest_id_before?: number
}

export type ListByFollowingResponse = {
  video_list: FeedVideoItem[]
  next_time: number
  has_more: boolean
}

export type IsLikedResponse = {
  is_liked: boolean
}

export type GetAllFollowersResponse = {
  followers: Account[]
}

export type GetAllVloggersResponse = {
  vloggers: Account[]
}
