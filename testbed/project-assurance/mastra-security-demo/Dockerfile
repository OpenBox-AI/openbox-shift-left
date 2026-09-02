FROM docker.io/library/node:26.7.0-bookworm-slim@sha256:4db36457f406501e6f608802e5da617e5fbd0e80b75901b6a09de1ae5a667d32 AS build

WORKDIR /app

# Reuse the conformance project's exact dependency lock and vendored OpenBox
# SDK artifacts. The demo changes application behavior, not its runtime tuple.
COPY mastra-conformance/package.json mastra-conformance/package-lock.json ./
COPY mastra-conformance/vendor ./vendor
RUN npm ci --ignore-scripts

COPY mastra-security-demo/src ./src
RUN npm run check \
    && npm run typecheck \
    && npm prune --omit=dev \
    && npm cache clean --force

FROM docker.io/library/node:26.7.0-bookworm-slim@sha256:4db36457f406501e6f608802e5da617e5fbd0e80b75901b6a09de1ae5a667d32

RUN apt-get update \
    && apt-get install --yes --no-install-recommends iproute2 nftables \
    && rm -rf /var/lib/apt/lists/*

LABEL org.opencontainers.image.title="OpenBox intentionally vulnerable Mastra security demo" \
      org.opencontainers.image.description="Synthetic prompt-injection and excessive-agency behavior for local OpenShell observation" \
      org.opencontainers.image.version="0.0.0" \
      ai.openbox.project-evaluation.contract="v1"

WORKDIR /app

COPY --from=build --chown=node:node /app/package.json /app/package-lock.json ./
COPY --from=build --chown=node:node /app/node_modules ./node_modules
COPY --from=build --chown=node:node /app/src ./src

ENV NODE_ENV=production

USER 1000:1000

ENTRYPOINT ["/usr/local/bin/node"]
CMD ["/app/src/index.mjs"]
