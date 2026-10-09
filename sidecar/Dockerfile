# ModelGateway sidecar 镜像。
#
# 构建（context 必须是仓库根：镜像里要放 backend 下的 proto 文件）：
#   docker build -f sidecar/Dockerfile -t feedflow-ai-sidecar .
#
# 为什么用 tsx 直接跑 TS 而不是先编译成 JS：
# 少一个构建阶段就少一处"本地跑的是 src、镜像里跑的是 dist"的漂移。
# 代价是运行期需要 tsx 与 typescript（见 devDependencies）——这是刻意接受的，
# 因为它换来的是"proto 与策略改动后镜像行为与本地一致"。
# 这个进程不参与主链路，多十几 MB 镜像不影响 API/Worker。
ARG NODE_VERSION=24-alpine

FROM node:${NODE_VERSION} AS deps
WORKDIR /app
# 只先拷 manifest：依赖层可以命中缓存，改业务代码不必重装依赖。
COPY sidecar/package.json sidecar/package-lock.json ./
RUN npm ci --no-audit --no-fund

FROM node:${NODE_VERSION} AS runtime
WORKDIR /app
ENV NODE_ENV=production \
    AI_GATEWAY_LISTEN_ADDR=0.0.0.0:50051 \
    AI_GATEWAY_PROTO_PATH=/app/api/proto/ai/v1/model_gateway.proto

COPY --from=deps /app/node_modules ./node_modules
COPY sidecar/package.json ./
COPY sidecar/tsconfig.json ./
COPY sidecar/src ./src
# 契约文件的唯一来源：与 Go 侧读的是同一个文件。
COPY backend/api/proto/ai/v1/model_gateway.proto ./api/proto/ai/v1/model_gateway.proto

EXPOSE 50051

# 刻意不写 HEALTHCHECK：
#   - sidecar 没有 HTTP 端口，健康判断要发一次 gRPC 调用，需要一个额外的探针脚本；
#   - 更重要的是 **健康状态不该被编排层用来重启它**——它随时可以被关掉，
#     主系统（API / Worker）完全不依赖它。
# 需要判断"能不能用"时请调 ModelGateway.Health，它返回的 serving 字段
# 已经区分了"活着但不提供服务"与"可用"两种情况。
CMD ["npx", "tsx", "src/server.ts"]
