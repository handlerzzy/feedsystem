/**
 * 计量与日志。
 *
 * 两件事必须同时做到，而且它们互相牵制：
 *   1. 每次调用都要有 token 与成本的记录（否则"AI 花了多少钱"永远说不清）；
 *   2. 记录里**绝不能**出现 API key（一旦落盘就是不可逆的泄漏）。
 *
 * 做法：只接受我们自己构造的结构化字段，绝不把整个请求/响应对象丢进日志。
 * 想加字段时请显式列出来——"顺手 log 一下上下文"正是密钥泄漏最常见的入口。
 */

export interface UsageRecord {
  readonly ts: string;
  readonly request_id: string;
  readonly method: string;
  readonly provider: string;
  readonly model: string;
  readonly prompt_tokens: number;
  readonly completion_tokens: number;
  readonly total_tokens: number;
  readonly cost_usd: number;
  readonly latency_ms: number;
  /** "ok" 或错误码；错误码来自 errors.ts 的白名单，不含自由文本。 */
  readonly outcome: string;
}

export interface UsageSink {
  record(rec: UsageRecord): void;
}

/**
 * 同时打日志与（可选）追加落盘的 sink。
 *
 * 为什么落盘用 JSONL 追加而不是数据库：这个进程不该引入任何存储依赖——
 * 它必须能被随时删掉而不影响主系统。一行一个 JSON 也便于事后用 jq 汇总。
 *
 * append 的实现由调用方注入（`fs.appendFile` 或 `fs.promises.appendFile`），
 * 因此这里**同时**处理两种失败方式：同步抛出，以及返回 Promise 后被 reject。
 * 只处理前者会让只读/写满的 /tmp 上的计量丢失得无声无息——
 * 而"这次 AI 到底花了多少钱"正是靠这份计量回答的。
 */
export function createUsageSink(
  logPath: string,
  appendFile: (path: string, data: string) => unknown,
): UsageSink {
  return {
    record(rec: UsageRecord): void {
      const line = JSON.stringify(rec);
      // 日志按 error 之外的级别打：计量是运维信息，不是故障。
      console.log(`[usage] ${line}`);
      if (logPath === "") return;
      try {
        const ret = appendFile(logPath, line + "\n");
        if (ret instanceof Promise) {
          ret.catch((err: unknown) => warnAppendFailed(err));
        }
      } catch (err) {
        warnAppendFailed(err);
      }
    },
  };
}

// 落盘失败只影响事后汇总，不影响服务本身，故为 warn 而非 error。
// 只打错误消息本身：计量记录里没有请求内容，但错误对象可能带路径以外的信息。
function warnAppendFailed(err: unknown): void {
  console.warn(`[usage] 计量落盘失败: ${err instanceof Error ? err.message : "unknown"}`);
}

/** 进程内的累计统计，供 Health 汇报（不含任何请求内容）。 */
export interface Metrics {
  calls: number;
  failures: number;
  totalTokens: number;
  totalCostUsd: number;
}

export function newMetrics(): Metrics {
  return { calls: 0, failures: 0, totalTokens: 0, totalCostUsd: 0 };
}
