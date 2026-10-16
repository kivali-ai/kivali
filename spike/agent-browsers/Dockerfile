# Throwaway spike image: Debian's Chromium, optionally under Xvfb.
FROM debian:trixie-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      chromium xvfb xauth \
      fonts-liberation fonts-noto-core fonts-noto-color-emoji \
      ca-certificates zstd procps dumb-init socat jq \
 && rm -rf /var/lib/apt/lists/*

RUN useradd --uid 1000 --create-home --shell /bin/sh chrome \
 && mkdir -p /profile && chown chrome:chrome /profile

COPY entrypoint.sh /entrypoint.sh
RUN chmod 0755 /entrypoint.sh

# MODE=headless (default) or MODE=xvfb. CHROMIUM_EXTRA_FLAGS for experiments.
ENV MODE=headless
# 9223 is socat forwarding to DevTools on 127.0.0.1:9222 (see entrypoint.sh).
EXPOSE 9223
ENTRYPOINT ["/usr/bin/dumb-init", "--"]
CMD ["/entrypoint.sh"]
