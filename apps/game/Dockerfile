# Build stage
FROM golang:1.23-alpine AS builder

WORKDIR /app
COPY . .

# Build the game application
RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=mod -o game apps/game/main.go

# Runtime stage
FROM alpine:3.18
RUN apk add --no-cache tzdata

RUN mkdir -p /app/server/apps/game
WORKDIR /app/server/apps/game

RUN /bin/cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime && echo 'Asia/Shanghai' >/etc/timezone

COPY --from=builder /app/game /app/server/apps/game/game

ENTRYPOINT [ "/app/server/apps/game/game" ]
