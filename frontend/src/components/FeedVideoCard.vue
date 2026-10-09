<script setup lang="ts">
import type { FeedVideoItem } from '../api/types'

const props = defineProps<{
  item: FeedVideoItem
  canLike: boolean
  busy?: boolean
}>()

const emit = defineEmits<{
  (e: 'toggle-like', item: FeedVideoItem): void
}>()

function onToggle() {
  emit('toggle-like', props.item)
}
</script>

<template>
  <div class="feed-card">
    <div class="cover">
      <img :src="item.cover_url" :alt="item.title" loading="lazy" />
    </div>
    <div class="content">
      <div class="row" style="justify-content: space-between">
        <div>
          <div class="title">
            <RouterLink :to="`/video/${item.id}`">{{ item.title }}</RouterLink>
          </div>
          <div class="subtle">
            作者：{{ item.author.username }} (#{{ item.author.id }}) · 创建时间：{{ new Date(item.create_time * 1000).toLocaleString() }}
          </div>
        </div>
        <div class="row">
          <span class="pill mono">❤️ {{ item.likes_count }}</span>
          <button
            v-if="canLike"
            class="primary"
            type="button"
            :disabled="busy"
            @click="onToggle"
            :title="item.is_liked ? '取消点赞' : '点赞'"
          >
            {{ item.is_liked ? '已赞' : '点赞' }}
          </button>
        </div>
      </div>
      <div v-if="item.reason" class="reason" data-testid="reason">
        <span class="reason-icon" aria-hidden="true">✦</span>{{ item.reason }}
      </div>
      <div v-if="item.summary" class="muted" style="margin-top: 8px">
        <span class="pill" style="margin-right: 6px">AI 摘要</span>{{ item.summary }}
      </div>
      <div v-if="item.description" class="muted" style="margin-top: 8px">{{ item.description }}</div>
      <div class="row" style="margin-top: 10px">
        <a class="pill mono" :href="item.play_url" target="_blank" rel="noreferrer">播放地址</a>
        <RouterLink class="pill" :to="`/video/${item.id}`">查看详情 / 评论</RouterLink>
      </div>
    </div>
  </div>
</template>

<style scoped>
.feed-card {
  display: grid;
  grid-template-columns: 240px minmax(0, 1fr);
  gap: 14px;
  border: 1px solid var(--border);
  background: rgba(255, 255, 255, 0.045);
  border-radius: var(--radius-lg);
  overflow: hidden;
  transition: border-color 200ms ease, transform 200ms ease, box-shadow 200ms ease;
}

.feed-card:hover {
  border-color: rgba(124, 92, 255, 0.42);
  transform: translateY(-2px);
  box-shadow: var(--shadow-lift);
}

.cover {
  background: rgba(0, 0, 0, 0.3);
  aspect-ratio: 16/9;
  overflow: hidden;
}

.cover img {
  width: 100%;
  height: 100%;
  object-fit: cover;
  display: block;
  transition: transform 320ms ease;
}

.feed-card:hover .cover img {
  transform: scale(1.04);
}

.content {
  padding: 12px 14px 14px;
  min-width: 0;
}

.content .title {
  font-size: 15px;
  font-weight: 700;
}

/*
 * 推荐理由：一行、低调、但可点开（title 里给出完整文案）。
 *
 * 为什么不用更醒目的样式：理由的作用是"让用户知道为什么看到这条"，
 * 而不是抢走标题与封面的注意力。它缺失时整行不渲染（v-if），
 * 因此不会留下一条空行。
 */
.reason {
  margin-top: 8px;
  font-size: 12px;
  line-height: 1.5;
  color: #c4b5fd;
  background: rgba(124, 92, 255, 0.12);
  border: 1px solid rgba(124, 92, 255, 0.28);
  border-radius: var(--radius-pill);
  padding: 2px 10px;
  display: inline-block;
  max-width: 100%;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.reason-icon {
  margin-right: 4px;
}

@media (max-width: 900px) {
  .feed-card {
    grid-template-columns: 1fr;
  }

  .feed-card:hover {
    transform: none;
  }
}
</style>
