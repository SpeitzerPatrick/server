# Build stage
FROM golang:1.23-alpine AS builder

WORKDIR /app
COPY . .

# Build the comment application
RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=mod -o comment apps/comment/main.go

# Runtime stage
FROM alpine:3.18
RUN apk add --no-cache tzdata

RUN mkdir -p /app/server/apps/comment
WORKDIR /app/server/apps/comment

RUN /bin/cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime && echo 'Asia/Shanghai' >/etc/timezone

COPY --from=builder /app/comment /app/server/apps/comment/comment

ENTRYPOINT [ "/app/server/apps/comment/comment" ]
