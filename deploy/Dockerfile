# 两阶段构建。基础镜像可通过 build-arg 替换——
# 很多 VPS 直连 Docker Hub 会超时，可用本地已有镜像当底座：
#   docker build --build-arg RUNTIME_BASE=nginx:1.27-alpine ...
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath \
      -ldflags="-s -w -linkmode external -extldflags -static" \
      -o /out/remote-storage-server ./server/cmd/remote-storage-server

ARG RUNTIME_BASE=alpine:3.20
FROM ${RUNTIME_BASE}
RUN (command -v apk >/dev/null && apk add --no-cache ca-certificates tzdata || true) \
 && (id -u locks >/dev/null 2>&1 || adduser -D -u 10001 locks || true) \
 && mkdir -p /data && chown locks:locks /data
COPY --from=build /out/remote-storage-server /usr/local/bin/remote-storage-server
USER locks
VOLUME ["/data"]
EXPOSE 8443
ENV REMOTE_STORAGE_ADDR=":8443" \
    REMOTE_STORAGE_DATA="/data/locks.db"
ENTRYPOINT ["/usr/local/bin/remote-storage-server"]
