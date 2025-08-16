# Build stage
FROM golang:1.23-alpine AS builder

WORKDIR /app
COPY . .

# Build the rank application
RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=mod -o rank apps/rank/main.go

# Runtime stage
FROM alpine:3.18
RUN apk add --no-cache tzdata

RUN mkdir -p /app/server/apps/rank
WORKDIR /app/server/apps/rank

RUN /bin/cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime && echo 'Asia/Shanghai' >/etc/timezone

COPY --from=builder /app/rank /app/server/apps/rank/rank

ENTRYPOINT [ "/app/server/apps/rank/rank" ]
