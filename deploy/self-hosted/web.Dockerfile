FROM node:24-alpine AS build
WORKDIR /src
COPY package.json package-lock.json ./
COPY apk/package.json apk/package.json
COPY packages/shared/package.json packages/shared/package.json
COPY e2e/package.json e2e/package.json
RUN npm ci --ignore-scripts --no-audit --no-fund
COPY apk/ apk/
COPY packages/ packages/
ARG PUBLIC_URL
ENV VITE_BASE=/app/ VITE_RELAY_BASE=${PUBLIC_URL}
RUN npm run build --workspace=tgcontrol-apk

FROM caddy:2-alpine
COPY --from=build /src/apk/dist/ /srv/app/
COPY deploy/self-hosted/Caddyfile /etc/caddy/Caddyfile
