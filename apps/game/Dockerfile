# 使用更小的基础镜像，或者使用已缓存的镜像
FROM alpine:3.18
RUN apk add --no-cache tzdata

RUN mkdir /app
RUN mkdir /app/server
RUN mkdir /app/server/apps
RUN mkdir /app/server/apps/game
WORKDIR /app/server/apps/game

RUN /bin/cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime && echo 'Asia/Shanghai' >/etc/timezone

ADD game /app/server/apps/game/game

ENTRYPOINT [ "/app/server/apps/game/game" ]
