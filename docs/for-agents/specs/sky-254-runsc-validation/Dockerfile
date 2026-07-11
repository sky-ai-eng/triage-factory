FROM debian:bookworm-slim

# Install runsc + a few binaries the probes need (wget for HTTPS,
# dnsutils for getent, ca-certificates for TLS verification).
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl wget dnsutils iproute2 iptables procps python3 \
  && curl -fsSL -o /usr/local/bin/runsc \
      https://storage.googleapis.com/gvisor/releases/release/latest/x86_64/runsc \
  && chmod +x /usr/local/bin/runsc \
  && rm -rf /var/lib/apt/lists/*

COPY probe.sh /probe.sh
RUN chmod +x /probe.sh

CMD ["/probe.sh"]
