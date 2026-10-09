import { postJson } from './client'
import { normalizeCommentList } from './normalize'
import type { Comment, MessageResponse } from './types'

export async function listAll(videoId: number) {
  const comments = await postJson<Comment[] | null>('/comment/listAll', { video_id: videoId })
  return normalizeCommentList(comments)
}

export function publish(videoId: number, content: string) {
  return postJson<MessageResponse>('/comment/publish', { video_id: videoId, content }, { authRequired: true })
}

export function remove(commentId: number) {
  return postJson<MessageResponse>('/comment/delete', { comment_id: commentId }, { authRequired: true })
}

/** 列表里的评论 id 序列是否完全一致（用于判断一次操作是否已经反映到列表里）。 */
function sameIds(a: Comment[], b: Comment[]) {
  if (a.length !== b.length) return false
  // b[i]?.id 而不是 b[i].id：项目开了 noUncheckedIndexedAccess，
  // 下标访问的类型是可能 undefined 的。长度已判等，所以这里只是让类型成立。
  return a.every((item, i) => item.id === b[i]?.id)
}

/**
 * 轮询列表，直到它相对 previous 发生变化（或次数用尽），返回最后一次的结果。
 *
 * 为什么需要它：**评论的写入与删除都是异步的**——`/comment/publish` 与
 * `/comment/delete` 只是把事件投进 RabbitMQ 就返回，真正改 MySQL 的是
 * CommentWorker。因此这两个接口返回后立刻调 listAll，拿到的是**操作之前**的
 * 状态。实测本机：发布后 T+0ms 列表里没有新评论，T+300ms 才出现。
 *
 * 表现到界面上就是"我刚发的评论看不见，要再发一条才看到上一条"。
 *
 * 用 id 序列比对而不是比长度：新增会让它变长、删除会让它变短，两种都覆盖；
 * 只比长度的话"删一条同时别人加一条"会被误判成没变化。
 *
 * 返回值里的 changed 用来区分两种结局：真的等到变化了，还是次数用尽仍没等到。
 * 后者不能报"已发布/已删除"——那正是用户会困惑的地方（提示说成功了，列表里却
 * 没有）。调用方据此给一句更准确的提示，并让用户用「刷新」重试。
 */
export async function listAllUntilChanged(
  videoId: number,
  previous: Comment[],
  tries = 8,
  intervalMs = 250,
): Promise<{ comments: Comment[]; changed: boolean }> {
  let last = await listAll(videoId)
  let changed = !sameIds(last, previous)
  for (let i = 0; i < tries && !changed; i++) {
    await new Promise((resolve) => setTimeout(resolve, intervalMs))
    last = await listAll(videoId)
    changed = !sameIds(last, previous)
  }
  return { comments: last, changed }
}
