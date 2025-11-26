FROM debian:trixie
LABEL org.opencontainers.image.source="https://github.com/maritimeconnectivity/mms"
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y ca-certificates && rm -rf /var/lib/apt/lists/* /var/cache/apt/archives/*
COPY router /app/router
RUN chmod +x /app/router
