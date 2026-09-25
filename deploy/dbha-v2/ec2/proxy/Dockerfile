FROM debian:bookworm-slim
ARG TARGETARCH=amd64
ARG PROXY_SHA256=f94a7a7168597d4e453ced832e413379d48a45ca2027e87c841b6b4f5bf7227a
RUN test "$TARGETARCH" = amd64
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl python3 libglib2.0-0 libevent-2.1-7 liblua5.1-0 && rm -rf /var/lib/apt/lists/*
RUN curl -fL --retry 5 --connect-timeout 20 --max-time 180 https://github.com/TencentBlueKing/blueking-dbm/releases/download/v1.0.0/mysql-proxy-0.82.15.tar.gz -o /tmp/proxy.tar.gz \
    && echo "$PROXY_SHA256  /tmp/proxy.tar.gz" | sha256sum -c - \
    && mkdir -p /opt/mysql-proxy && tar -xzf /tmp/proxy.tar.gz -C /opt/mysql-proxy --strip-components=1 && rm /tmp/proxy.tar.gz
ENV LD_LIBRARY_PATH=/opt/mysql-proxy/lib:/opt/mysql-proxy/glib-2.0/lib:/opt/mysql-proxy/libevent-2.0.15-stable/lib
COPY entrypoint.sh /entrypoint.sh
COPY proxy-supervisor.py /usr/local/bin/proxy-supervisor.py
RUN chmod +x /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
