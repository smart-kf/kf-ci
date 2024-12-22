FROM alpine
RUN apk add --no-cache tzdata
ENV TZ=Asia/Shanghai
WORKDIR /app
COPY bin/ci /app/
COPY web /app/web
