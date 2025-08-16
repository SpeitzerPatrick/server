# Build stage
FROM golang:1.23-alpine AS builder

WORKDIR /app
COPY . .

# Build the mail application
RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=mod -o mail apps/mail/main.go

# Runtime stage
FROM alpine:3.18
RUN apk add --no-cache tzdata

RUN mkdir -p /app/server/apps/mail
WORKDIR /app/server/apps/mail

RUN /bin/cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime && echo 'Asia/Shanghai' >/etc/timezone

COPY --from=builder /app/mail /app/server/apps/mail/mail

ENTRYPOINT [ "/app/server/apps/mail/mail" ]
