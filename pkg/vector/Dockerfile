FROM timberio/vector:0.45.0-alpine

COPY etc/vector.toml /etc/vector/vector.toml

ENTRYPOINT ["vector", "-w", "--config", "/etc/vector/vector.toml"]
