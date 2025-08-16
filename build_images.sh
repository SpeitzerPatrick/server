#!/bin/bash

# 构建所有应用镜像的脚本

# 需要构建的应用列表
APPS=("gate" "rank" "comment" "client_bots")

# 通用的Dockerfile模板
DOCKERFILE_TEMPLATE='# Build stage
FROM golang:1.23-alpine AS builder

WORKDIR /app
COPY . .

# Build the APPLICATION application
RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=mod -o APPLICATION apps/APPLICATION/main.go

# Runtime stage
FROM alpine:3.18
RUN apk add --no-cache tzdata

RUN mkdir -p /app/server/apps/APPLICATION
WORKDIR /app/server/apps/APPLICATION

RUN /bin/cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime && echo '\''Asia/Shanghai'\'' >/etc/timezone

COPY --from=builder /app/APPLICATION /app/server/apps/APPLICATION/APPLICATION

ENTRYPOINT [ "/app/server/apps/APPLICATION/APPLICATION" ]'

# 为每个应用创建Dockerfile并构建镜像
for app in "${APPS[@]}"; do
    echo "构建 $app 镜像..."
    
    # 创建应用特定的Dockerfile
    dockerfile_content=$(echo "$DOCKERFILE_TEMPLATE" | sed "s/APPLICATION/$app/g")
    echo "$dockerfile_content" > "apps/$app/Dockerfile"
    
    # 构建镜像
    sudo docker build -f "apps/$app/Dockerfile" -t "$app:latest" .
    
    if [ $? -eq 0 ]; then
        echo "$app 镜像构建成功!"
    else
        echo "$app 镜像构建失败!"
        exit 1
    fi
done

echo "所有镜像构建完成!"
