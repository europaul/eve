FROM victoriametrics/vmagent:v1.117.1

# Copy your local vmagent config into the container
COPY etc/vmagent.yml /etc/vmagent.yml

# Copy the run.sh script into the container
COPY run.sh /run.sh

# Make the run.sh script executable
RUN chmod +x /run.sh

ENTRYPOINT ["/run.sh"]
