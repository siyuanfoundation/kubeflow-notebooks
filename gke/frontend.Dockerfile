FROM node:22-bookworm-slim AS builder
WORKDIR /src
COPY workspaces/frontend/package*.json ./
RUN npm ci
COPY workspaces/frontend/ ./
ENV DEPLOYMENT_MODE=standalone
ENV PUBLIC_PATH=/workspaces/
RUN npm run build:prod

FROM nginxinc/nginx-unprivileged:1.28-alpine
COPY --from=builder /src/dist /usr/share/nginx/html/workspaces
COPY --chmod=644 gke/frontend-nginx.conf /etc/nginx/conf.d/default.conf
USER 101:101
EXPOSE 8080
