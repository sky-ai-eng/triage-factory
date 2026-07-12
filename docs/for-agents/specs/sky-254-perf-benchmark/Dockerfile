FROM debian:bookworm-slim

# Same runsc base as the validation probe, plus `time` for elapsed
# measurements and `bc` for stats arithmetic.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl python3 time bc \
  && curl -fsSL -o /usr/local/bin/runsc \
      https://storage.googleapis.com/gvisor/releases/release/latest/x86_64/runsc \
  && chmod +x /usr/local/bin/runsc \
  && rm -rf /var/lib/apt/lists/*

COPY bench.sh /bench.sh
RUN chmod +x /bench.sh

# Stay alive so we can ssh in and invoke the benchmark interactively.
CMD ["sleep", "infinity"]
