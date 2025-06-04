FROM timberio/vector:0.45.0-alpine
# FROM paulzededa/eve-vector:0.0.1

COPY etc/vector.yaml /etc/vector/vector.yaml
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

ENV VECTOR_LOG="vector=info,vector::sources::util::unix_stream=warn"

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
