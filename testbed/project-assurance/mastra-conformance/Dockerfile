FROM docker.io/library/node:26.7.0-bookworm-slim@sha256:4db36457f406501e6f608802e5da617e5fbd0e80b75901b6a09de1ae5a667d32 AS build

WORKDIR /app

COPY package.json package-lock.json ./
COPY vendor ./vendor
RUN npm ci --ignore-scripts

COPY src ./src
RUN npm run check \
    && npm run typecheck \
    && npm prune --omit=dev \
    && npm cache clean --force

FROM docker.io/library/node:26.7.0-bookworm-slim@sha256:4db36457f406501e6f608802e5da617e5fbd0e80b75901b6a09de1ae5a667d32

RUN apt-get update \
    && apt-get install --yes --no-install-recommends iproute2 nftables \
    && rm -rf /var/lib/apt/lists/*

LABEL org.opencontainers.image.title="OpenBox Mastra project-evaluation test image" \
      org.opencontainers.image.description="Mastra 1.8.0 project with OpenBox Mastra SDK 1.0.0 for local OpenShell evaluation" \
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
