# chrony as a hardened, non-root, distroless NTP/NTS server.
#
#   * chronyd and chronyc are built as static PIE binaries against musl, so the
#     final image can be gcr.io/distroless/static-debian13:nonroot (no shell, no libc,
#     no package manager, nothing to upgrade at runtime).
#   * NTS needs GnuTLS, which no distribution ships as a static library, so
#     GnuTLS is built here with its bundled libtasn1/libunistring and without
#     p11-kit, IDN, zlib, brotli and zstd.
#   * Everything is pinned by version and SHA-256.
#
# Build:
#   docker buildx build --platform linux/amd64,linux/arm64 -t chrony .

ARG ALPINE_VERSION=3.24
ARG GO_VERSION=1.27
ARG DISTROLESS_IMAGE=gcr.io/distroless/static-debian13:nonroot
ARG DISTROLESS_DIGEST=sha256:516757106de35205efb70b91ba03f6f1298424a0678c603255cf2852a7000087

# ---------------------------------------------------------------------------
# GnuTLS, static
# ---------------------------------------------------------------------------
FROM alpine:${ALPINE_VERSION} AS gnutls
ARG GNUTLS_SERIES=3.8
ARG GNUTLS_VERSION=3.8.13
ARG GNUTLS_SHA256=ffed8ec1bf09c2426d4f14aae377de4753b53e537d685e604e99a8b16ca9c97e

RUN apk add --no-cache build-base curl xz pkgconf linux-headers \
        gmp-dev gmp-static nettle-dev nettle-static

WORKDIR /build
RUN curl -fsSLO "https://www.gnupg.org/ftp/gcrypt/gnutls/v${GNUTLS_SERIES}/gnutls-${GNUTLS_VERSION}.tar.xz" \
 && echo "${GNUTLS_SHA256}  gnutls-${GNUTLS_VERSION}.tar.xz" | sha256sum -c - \
 && tar xf "gnutls-${GNUTLS_VERSION}.tar.xz"

WORKDIR /build/gnutls-${GNUTLS_VERSION}
RUN CFLAGS="-O2 -fstack-protector-strong -fPIE -D_FORTIFY_SOURCE=2" \
    ./configure \
        --prefix=/opt/gnutls \
        --disable-shared --enable-static \
        --with-included-libtasn1 --with-included-unistring \
        --without-p11-kit --without-idn --without-tpm --without-tpm2 \
        --without-zlib --without-brotli --without-zstd \
        --disable-libdane --disable-cxx --disable-tools --disable-doc \
        --disable-tests --disable-full-test-suite --disable-nls --disable-rpath \
        --with-default-trust-store-file=/etc/ssl/certs/ca-certificates.crt \
 && make -j"$(nproc)" \
 && make install

# ---------------------------------------------------------------------------
# chrony, static PIE
# ---------------------------------------------------------------------------
FROM alpine:${ALPINE_VERSION} AS chrony
ARG CHRONY_VERSION=4.9
ARG CHRONY_SHA256=4924c6f530105bcd5b9e9e33c48a2ae1bfd889222c8480bc41601110efc864d0

RUN apk add --no-cache build-base curl pkgconf tzdata linux-headers \
        gmp-dev gmp-static nettle-dev nettle-static \
        libcap-dev libcap-static libseccomp-dev libseccomp-static
COPY --from=gnutls /opt/gnutls /opt/gnutls

WORKDIR /build
RUN curl -fsSLO "https://chrony-project.org/releases/chrony-${CHRONY_VERSION}.tar.gz" \
 && echo "${CHRONY_SHA256}  chrony-${CHRONY_VERSION}.tar.gz" | sha256sum -c - \
 && tar xzf "chrony-${CHRONY_VERSION}.tar.gz"

WORKDIR /build/chrony-${CHRONY_VERSION}
# --enable-scfilter        seccomp system call filtering (chronyd -F)
# privdrop + libcap kept   so that chronyd still drops privileges if someone
#                          runs the image as root
# --disable-readline/rtc   drop code and syscalls that are useless in a container
# chrony's configure asks pkg-config for the library flags; GnuTLS is only
# available as a static archive here, so pkg-config must run with --static or
# the link test fails and NTS is silently dropped.
RUN printf '#!/bin/sh\nexec /usr/bin/pkg-config --static "$@"\n' > /usr/local/bin/pkg-config-static \
 && chmod +x /usr/local/bin/pkg-config-static

RUN PKG_CONFIG=/usr/local/bin/pkg-config-static \
    PKG_CONFIG_PATH=/opt/gnutls/lib/pkgconfig \
    CFLAGS="-O2 -fstack-protector-strong -fPIE -D_FORTIFY_SOURCE=2" \
    LDFLAGS="-static-pie -Wl,-z,relro,-z,now,-z,noexecstack" \
    ./configure \
        --prefix=/usr \
        --sbindir=/usr/local/sbin \
        --sysconfdir=/etc/chrony \
        --with-user=nonroot \
        --with-pidfile=/run/chrony/state/chronyd.pid \
        --chronyrundir=/run/chrony/state \
        --enable-scfilter \
        --disable-readline --without-editline \
        --disable-rtc --without-nss --without-tomcrypt \
 && make -j"$(nproc)" \
 && make install DESTDIR=/out \
 && strip /out/usr/local/sbin/chronyd /out/usr/bin/chronyc

# Fail the build rather than ship an image that silently lost NTS, the seccomp
# filter, privilege dropping, or that turned out not to be static.
RUN set -eux; \
    /out/usr/local/sbin/chronyd -v; \
    /out/usr/local/sbin/chronyd -v | grep -q -- '+NTS'; \
    /out/usr/local/sbin/chronyd -v | grep -q -- '+SCFILTER'; \
    /out/usr/local/sbin/chronyd -v | grep -q -- '+PRIVDROP'; \
    /out/usr/local/sbin/chronyd -v | grep -q -- '+SECHASH'; \
    ! readelf -lW /out/usr/local/sbin/chronyd | grep -q INTERP; \
    ! readelf -lW /out/usr/bin/chronyc | grep -q INTERP; \
    readelf -hW /out/usr/local/sbin/chronyd | grep -q 'DYN (Position-Independent'

# Empty, correctly owned state directory for the final image, plus the leap
# second table so a stratum-1 or orphaned server announces leap seconds.
RUN install -d -m 0750 -o 65532 -g 65532 /files/run/chrony \
 && install -d -m 0700 -o 65532 -g 65532 /files/run/chrony/state \
 && install -D -m 0644 /usr/share/zoneinfo/leap-seconds.list /files/usr/share/zoneinfo/leap-seconds.list

# ---------------------------------------------------------------------------
# entrypoint
# ---------------------------------------------------------------------------
FROM golang:${GO_VERSION}-alpine AS entrypoint
ARG VERSION=dev
WORKDIR /src
COPY entrypoint/go.mod ./
COPY entrypoint/*.go ./
RUN apk add --no-cache binutils
RUN CGO_ENABLED=0 go vet ./... \
 && CGO_ENABLED=0 go test ./... \
 && CGO_ENABLED=0 go build -trimpath \
        -ldflags "-s -w -buildid= -X main.version=${VERSION}" \
        -o /chrony-entrypoint . \
 && ! readelf -lW /chrony-entrypoint | grep -q INTERP

# ---------------------------------------------------------------------------
# final image
# ---------------------------------------------------------------------------
FROM ${DISTROLESS_IMAGE}@${DISTROLESS_DIGEST}
ARG VERSION=dev
ARG CHRONY_VERSION=4.9

LABEL org.opencontainers.image.title="chrony" \
      org.opencontainers.image.description="Hardened non-root distroless chrony NTP/NTS server" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.source="https://github.com/MaximeWewer/chrony-docker"

# Deliberately not /usr/sbin/chronyd. Debian/Ubuntu hosts ship an AppArmor
# profile attached to that path, which also confines unconfined container
# processes executing it. That profile has no signal rules, so it drops the
# SIGTERM/SIGKILL sent by runc: chronyd never shuts down cleanly, loses its
# state, and is killed after the grace period (exit 137).
COPY --from=chrony /out/usr/local/sbin/chronyd /usr/local/sbin/chronyd
COPY --from=chrony /out/usr/bin/chronyc  /usr/bin/chronyc
COPY --from=chrony /files/ /
COPY --from=entrypoint /chrony-entrypoint /chrony-entrypoint

ENV CHRONY_VERSION=${CHRONY_VERSION} \
    CHRONY_STATE_DIR=/run/chrony/state \
    NTP_PORT=123 \
    NTS_PORT=4460 \
    SOURCE_OPTIONS="iburst minpoll 6 maxpoll 10" \
    SCFILTER_LEVEL=1 \
    CLOCK_CONTROL=false

USER 65532:65532
# Not the state directory: if it is missing on a freshly mounted volume, the
# runtime would create the working directory as root before the entrypoint
# runs, and the non-root entrypoint could neither write to it nor fix it.
WORKDIR /
VOLUME ["/run/chrony"]
EXPOSE 123/udp 4460/tcp

HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
    CMD ["/chrony-entrypoint", "health"]

ENTRYPOINT ["/chrony-entrypoint"]
CMD ["run"]
