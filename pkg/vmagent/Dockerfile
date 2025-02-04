FROM victoriametrics/vmagent:latest

# Copy your local vmagent config into the container
COPY etc/vmagent.yml /etc/vmagent.yml

CMD ["-promscrape.config=/etc/vmagent.yml", "-remoteWrite.url=http://localhost:8428/api/v1/write"]
