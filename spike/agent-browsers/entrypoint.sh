#!/bin/sh
# Restores /profile.tar.zst into /profile (if present), then runs Chromium as
# uid 1000. After Chromium exits the container stays up (sleep) so the harness
# can pack the profile with `docker exec`.
set -eu
set -f

mkdir -p /profile
if [ -f /profile.tar.zst ]; then
  zstd -dcq /profile.tar.zst | tar -xf - -C /profile
  echo "restored profile from /profile.tar.zst"
fi
# Singleton* point at the old container's hostname; Chromium would refuse to start.
rm -f /profile/SingletonLock /profile/SingletonCookie /profile/SingletonSocket
chown -R chrome:chrome /profile
rm -f /tmp/chromium-exited

FLAGS="--user-data-dir=/profile --password-store=basic --no-first-run --no-default-browser-check \
--disable-background-networking --remote-debugging-port=9222 --remote-debugging-address=0.0.0.0 \
--remote-allow-origins=* --window-size=1280,800 ${CHROMIUM_EXTRA_FLAGS:-}"

# Docker's default seccomp profile blocks user namespaces, so Chromium's
# sandbox cannot start ("No usable sandbox!"). Unless SANDBOX=1 (which needs a
# seccomp profile allowing clone/unshare, see chrome-seccomp.json), run with
# --no-sandbox and rely on the container as the isolation boundary.
if [ "${SANDBOX:-0}" != "1" ]; then
  FLAGS="$FLAGS --no-sandbox"
fi

# This Chromium ignores --remote-debugging-address and binds DevTools to
# 127.0.0.1:9222 inside the container, so socat exposes it on 0.0.0.0:9223.
socat TCP-LISTEN:9223,fork,reuseaddr,bind=0.0.0.0 TCP:127.0.0.1:9222 &

# Run the real binary, not /usr/bin/chromium: the Debian wrapper silently adds
# --disable-dev-shm-usage whenever /dev/shm is under 3.8 GB, plus other flags.
set +e
if [ "${MODE:-headless}" = "xvfb" ]; then
  echo "starting chromium (xvfb)"
  runuser -u chrome -- xvfb-run -a -s "-screen 0 1280x800x24" /usr/lib/chromium/chromium $FLAGS about:blank
else
  echo "starting chromium (headless=new)"
  runuser -u chrome -- /usr/lib/chromium/chromium --headless=new $FLAGS about:blank
fi
echo "chromium exited with $?"
touch /tmp/chromium-exited
exec sleep infinity
