# The Alpacaruns API image.
#
# The Go binary is cross-compiled and STATIC (CGO_ENABLED=0), so the
# image needs no libc: a scratch base plus the binary and a CA bundle
# is the whole runtime. The CA bundle is what lets the API talk to
# Alpaca over HTTPS, and it is copied in rather than installed from a
# package manager so the image build is deterministic and offline.
#
# Build context: this directory plus the binary. The binary is copied
# in by deploy/edge/build.sh, which is where the cross-compile happens.

FROM scratch

COPY alpacaruns.linux /usr/local/bin/alpacaruns
COPY ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

# The API writes the pause flag into the mounted data directory; the
# bot writes the journal. Both run as the same uid, which the compose
# file passes in.
USER 65532:65532

ENTRYPOINT ["/usr/local/bin/alpacaruns"]
