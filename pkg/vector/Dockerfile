FROM lfedge/eve-vector-base:X

RUN apk add --no-cache inotify-tools

COPY etc/vector.yaml /etc/vector/vector.yaml
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
