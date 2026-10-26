# syntax=docker/dockerfile:1

# ---------- build ----------
FROM node:24.21.0-alpine3.24 AS build
WORKDIR /app
COPY package.json package-lock.json .npmrc ./
RUN npm ci --no-audit --no-fund
COPY tsconfig.json vite.config.ts index.html ./
COPY public ./public
COPY src ./src
RUN npm run build

# ---------- runtime ----------
FROM nginxinc/nginx-unprivileged:1.31.7-alpine3.24
# Origin Zitadel для CSP (connect-src / frame-src). Переопределяется в compose.
ENV ZITADEL_ORIGIN=http://localhost:8081
COPY nginx.conf /etc/nginx/templates/default.conf.template
COPY security-headers.conf /etc/nginx/templates/snippets/security-headers.conf.template
COPY --from=build /app/dist /usr/share/nginx/html/dashboard
USER 101
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
