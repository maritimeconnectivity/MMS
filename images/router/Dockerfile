FROM --platform=linux/amd64 debian:bookworm
RUN apt-get update && apt-get install -y ca-certificates
COPY router /app/router
RUN chmod +x /app/router