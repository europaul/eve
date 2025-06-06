FROM timberio/vector:0.45.0-alpine
# FROM paulzededa/eve-vector:0.0.1

COPY etc/vector.yaml /etc/vector/vector.yaml
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
