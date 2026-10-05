FROM alpine:3.21
RUN apk add --no-cache pgbouncer su-exec \
    && addgroup -S pgbouncer \
    && adduser -S -G pgbouncer pgbouncer
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
