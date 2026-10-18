# ShanHaiGateway (shg) — 多阶段构建，纯 Go SQLite（modernc.org/sqlite，零 CGO）。
# 真实配置与数据不进镜像：运行时挂载 /etc/shg（配置）与 /var/lib/shg（数据）。

# ── 构建段 ─────────────────────────────────────────────────────
FROM golang:1.27-alpine AS build
# 模块代理默认走国内镜像(宿主 go env 同款)；海外构建可覆盖：
#   docker build --build-arg GOPROXY=https://proxy.golang.org,direct ...
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src
# 先拷依赖清单，充分利用层缓存
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/shg ./cmd/shg

# ── 运行段 ─────────────────────────────────────────────────────
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -g 10001 shg \
    && adduser -D -u 10001 -G shg shg

COPY --from=build /out/shg /usr/local/bin/shg

# 路径语义与 README 四一致：env 可覆盖，默认指向容器内约定位置
ENV GATEWAY_CONFIG_DIR=/etc/shg \
    GATEWAY_DATA_DIR=/var/lib/shg \
    GATEWAY_DB_FILE=/var/lib/shg/shg.db \
    LISTEN=:18006
EXPOSE 18006

USER shg
VOLUME ["/etc/shg", "/var/lib/shg"]

# 网关健康检查（README 三：GET /health）
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:18006/health >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/usr/local/bin/shg"]
