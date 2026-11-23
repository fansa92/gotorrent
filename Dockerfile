# gotorrent 容器镜像
#
# 构建:  docker build -t gotorrent .
# 运行:  docker run -d --name gotorrent \
#           -p 6800:6800 \
#           -v "$PWD/downloads:/downloads" \
#           -v "$PWD/config:/config" \
#           gotorrent --daemon --rpc-listen 0.0.0.0:6800
#
# 镜像特点:
#   - 多阶段构建:编译阶段用 golang 工具链,运行阶段换 alpine,体积小。
#   - 静态编译(CGO_ENABLED=0),运行镜像无需 glibc。
#   - 非 root 用户运行,降低容器逃逸风险。
#   - 卷:/downloads 下载目录,/config 配置与历史库。

# ---- 编译阶段 ----
FROM golang:1.26-alpine AS build

# git 由 go mod 下载部分依赖时需要;ca-certificates 供 HTTPS 拉取
RUN apk add --no-cache git ca-certificates

WORKDIR /src

# 先拷 go.mod / go.sum 再下载依赖,利用 Docker 层缓存:
# 只要这两个文件不变,依赖层不会重建。
COPY go.mod go.sum ./
RUN go mod download

# 再拷源码编译
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags="-s -w -X main.version=$(git describe --tags --always 2>/dev/null || echo dev)" \
      -o /out/gotorrent .

# ---- 运行阶段 ----
FROM alpine:3.20

LABEL org.opencontainers.image.title="gotorrent"
LABEL org.opencontainers.image.description="基于 anacrolix/torrent 的 aria2 风格 BitTorrent 下载工具"
LABEL org.opencontainers.image.licenses="MIT"

# ca-certificates: magnet 元数据与 WebSeed 可能走 HTTPS
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S gotorrent \
    && adduser -S -G gotorrent -h /downloads -s /sbin/nologin gotorrent \
    && mkdir -p /downloads /config \
    && chown -R gotorrent:gotorrent /downloads /config

COPY --from=build /out/gotorrent /usr/local/bin/gotorrent

# 端口:6800 控制面(RPC/REST/SSE/Web UI),流服务与 DHT/监听端口按需另行发布
EXPOSE 6800

# 下载与配置卷。配置文件固定在 $HOME/.gotorrent/config,
# 因此把 HOME 指到 /config 卷,持久化配置;历史库位于下载目录的 .gotorrent/ 子目录。
ENV HOME=/config
VOLUME ["/downloads", "/config"]

# 非 root 运行
USER gotorrent

WORKDIR /downloads

# 默认常驻守护模式;控制面监听 0.0.0.0 才能从容器外访问
# 下载目录用 -d 指到 /downloads;配置走 $HOME/.gotorrent/config(即 /config 卷)
ENTRYPOINT ["/usr/local/bin/gotorrent"]
CMD ["--daemon", "--rpc-listen", "0.0.0.0:6800", "-d", "/downloads"]

# 健康检查说明(不在镜像内定义 HEALTHCHECK,避免依赖 curl/wget):
#   docker run 时可加 --health-cmd 或在 compose 里用 wget 探测 /ui
