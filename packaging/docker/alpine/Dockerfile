FROM docker.io/library/golang:bookworm AS build
RUN apt-get update && apt-get install -y --no-install-recommends build-essential git make && rm -rf /var/lib/apt/lists/*
ADD . /go/src/app
WORKDIR /go/src/app
# PIE build dynamically links libc — must compile on glibc (Debian) to match runtime.
RUN make miniflux

FROM docker.io/library/node:22-alpine AS defuddle
RUN apk add --no-cache git
RUN git clone --depth 1 https://github.com/kepano/defuddle /defuddle
WORKDIR /defuddle
RUN npm ci && npm run build

# Debian slim keeps the native headless browser on glibc instead of Alpine musl.
FROM docker.io/library/debian:bookworm-slim

LABEL org.opencontainers.image.title=Miniflux
LABEL org.opencontainers.image.description="Miniflux is a minimalist and opinionated feed reader"
LABEL org.opencontainers.image.licenses=Apache-2.0
LABEL org.opencontainers.image.source=https://github.com/naiba-forks/miniflux

EXPOSE 8080
ENV LISTEN_ADDR=0.0.0.0:8080
# TARGETARCH is provided by Docker buildx (amd64/arm64). Miniflux only needs
# HTML/JS execution, so use Obscura's smaller no-render build with the full
# stealth feature compiled in.
ARG TARGETARCH
ARG OBSCURA_VERSION=v0.2.5
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata tini nodejs curl \
    && rm -rf /var/lib/apt/lists/* \
    && ARCH=$(echo "${TARGETARCH}" | sed 's/amd64/x86_64/;s/arm64/aarch64/') \
    && case "${ARCH}" in \
         x86_64) OBSCURA_SHA256=7f7b23dbb40622784729e68be038f2893327bfab255eb6894dad47dd9b18cbd2 ;; \
         aarch64) OBSCURA_SHA256=934e1deb8ed0f4bf98a11e7e23c4736b5a7e414f50ab17fc5495778fe25bf2e5 ;; \
         *) echo "Unsupported architecture: ${TARGETARCH}" >&2; exit 1 ;; \
       esac \
    && curl -fsSL -o /tmp/obscura.tar.gz \
       "https://github.com/h4ckf0r0day/obscura/releases/download/${OBSCURA_VERSION}/obscura-${ARCH}-linux-no-render-stealth.tar.gz" \
    && echo "${OBSCURA_SHA256}  /tmp/obscura.tar.gz" | sha256sum -c - \
    && tar -xzf /tmp/obscura.tar.gz -C /usr/bin obscura obscura-worker \
    && rm /tmp/obscura.tar.gz \
    && chmod +x /usr/bin/obscura /usr/bin/obscura-worker
COPY --from=build /go/src/app/miniflux /usr/bin/miniflux
COPY --from=defuddle /defuddle/dist /usr/share/miniflux/defuddle/dist
COPY --from=defuddle /defuddle/node_modules /usr/share/miniflux/defuddle/node_modules
COPY --from=defuddle /defuddle/package.json /usr/share/miniflux/defuddle/package.json
USER 65534
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["/usr/bin/miniflux"]
