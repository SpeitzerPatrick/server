# Build stage
FROM golang:1.23-alpine AS builder

WORKDIR /app
COPY . .

# Build the gate application
RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=mod -o gate apps/gate/main.go

# Runtime stage
FROM alpine:3.18
RUN apk add --no-cache tzdata

RUN mkdir -p /app/server/apps/gate
WORKDIR /app/server/apps/gate

RUN /bin/cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime && echo 'Asia/Shanghai' >/etc/timezone

COPY --from=builder /app/gate /app/server/apps/gate/gate

ENTRYPOINT [ "/app/server/apps/gate/gate" ]
