/**
 * 错误与 gRPC 状态码的映射。
 *
 * 为什么要在本进程内定义一套自己的错误码，而不是直接把异常往上抛：
 * Go 侧需要**区分**几种完全不同的处置方式（见 internal/ai/client.go）：
 *   - sidecar 本来就没开 → 静默降级，不该刷告警；
 *   - provider 暂时故障 → 记 Warn、可以稍后重试；
 *   - 请求本身有问题 → 是我们自己的 bug，必须响亮。
 * 如果都变成 codes.Unknown + 一段文本，这个区分就只能靠匹配字符串，
 * 而字符串匹配会在第一次改文案时静默失效。
 *
 * 另一条纪律：**错误消息里绝不能出现 API key**。
 * 因此这里只接受调用方给的短语；凡是可能带凭据的对象（pi-ai 的 auth）
 * 都不允许出现在消息里（见 models.ts 的注释）。
 */

import { status as GrpcStatus } from "@grpc/grpc-js";

export type GatewayErrorCode =
  | "disabled" // 总开关关闭
  | "provider_unavailable" // 没有任何可用 provider（缺 key / 未配置）
  | "model_not_found" // 模型名不在目录里
  | "provider_error" // provider 侧失败（额度、限流、上游 5xx…）
  | "invalid_response" // 模型返回了不可用的内容（空、非法 JSON）
  | "timeout" // 调用超时
  | "bad_request"; // 请求本身不合法

export class GatewayError extends Error {
  readonly code: GatewayErrorCode;

  constructor(code: GatewayErrorCode, message: string) {
    super(message);
    this.name = "GatewayError";
    this.code = code;
  }
}

/** 把内部错误码映射成 gRPC 状态码。 */
export function grpcCodeFor(code: GatewayErrorCode): GrpcStatus {
  switch (code) {
    case "disabled":
      // FailedPrecondition 而不是 Unavailable：这是"配置决定的正常状态"，
      // 不是故障。Go 侧据此直接降级，不记告警。
      return GrpcStatus.FAILED_PRECONDITION;
    case "provider_unavailable":
      return GrpcStatus.UNAVAILABLE;
    case "model_not_found":
      return GrpcStatus.NOT_FOUND;
    case "timeout":
      return GrpcStatus.DEADLINE_EXCEEDED;
    case "bad_request":
      return GrpcStatus.INVALID_ARGUMENT;
    case "invalid_response":
    case "provider_error":
    default:
      // provider 出错对我们而言是"上游依赖失败"，UNKNOWN 之外没有更贴切的码；
      // 用 INTERNAL 会让人以为是 sidecar 自己的 bug。
      return GrpcStatus.UNKNOWN;
  }
}

/**
 * 把任意抛出物归一化成 {code, message}，**并保证消息里不含凭据**。
 *
 * 关键点：**所有**分支都要过 sanitize，包括"已经是 GatewayError"的那一支。
 * 这里曾经是 review 抓到的真实漏洞：models.ts 会把 provider SDK 的错误文本
 * （4xx/5xx 响应体里常常带 key 片段，例如
 * `openai 401 Incorrect API key provided: sk-proj-XXXX`）直接塞进
 * GatewayError 的 message，而当时这一支 `return err` 原样放行，sanitize 被跳过；
 * server.ts 再把它原样放进 gRPC status 的 message 与 details，
 * 最终出现在 Go 侧日志里——一条不可逆的密钥泄漏。
 *
 * 教训写在这里：**清洗必须发生在唯一的出口上**，
 * 不能依赖"调用方会构造干净的错误"。所以下面无论是哪一支，都重新构造一次。
 */
export function toGatewayError(err: unknown): GatewayError {
  if (err instanceof GatewayError) {
    return new GatewayError(err.code, sanitize(err.message));
  }
  if (err instanceof Error) {
    return new GatewayError("provider_error", sanitize(err.message));
  }
  return new GatewayError("provider_error", "未知错误");
}

/**
 * 去掉消息里形似密钥的片段。
 *
 * 这一层"多此一举"的清洗是刻意的：错误消息最终会进入 Go 侧日志，
 * 而 provider SDK 在 4xx/5xx 时**有可能**把请求头或 key 片段带进错误文本。
 * 与其依赖上游的自觉，不如在这里统一剪掉——密钥泄漏是不可逆的，
 * 多一次 replace 的成本可以忽略。
 *
 * 覆盖的形态（都是在真实 provider 报错里见过的）：
 *   - `sk-...` / `sk-proj-...` / `key-...` / `api_key-...` 这类带前缀的 token；
 *   - `Bearer <token>`；
 *   - 32 位以上的裸十六进制串（部分 provider 用它当 key）。
 */
export function sanitize(message: string): string {
  return message
    .replace(/\b(sk|pk|api|key)[-_][A-Za-z0-9_-]{6,}\b/gi, "[redacted]")
    .replace(/\bBearer\s+[A-Za-z0-9._-]{8,}\b/gi, "Bearer [redacted]")
    .replace(/\b[A-Fa-f0-9]{32,}\b/g, "[redacted]");
}
