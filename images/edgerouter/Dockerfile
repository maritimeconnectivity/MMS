FROM --platform=linux/amd64 debian:bookworm
RUN apt-get update && apt-get install -y ca-certificates
COPY edgerouter /app/edgerouter
RUN chmod +x /app/edgerouter