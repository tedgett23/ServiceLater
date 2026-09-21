# One image for every role. Build context: repo root.
FROM node:22 AS web
WORKDIR /src/web
COPY web/package*.json ./
RUN npm ci
COPY web/ ./
# adapter-static writes to ../backend/internal/webui/dist
RUN mkdir -p ../backend/internal/webui && npm run build

FROM golang:1.24 AS build
# The first-party apps are a Go module of their own (they embed themselves), which the backend module points at.
COPY apps /src/apps
WORKDIR /src/backend
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
COPY --from=web /src/backend/internal/webui/dist ./internal/webui/dist
RUN CGO_ENABLED=0 go build -o /out/servicelater ./cmd/servicelater

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/servicelater /servicelater
# The first-party apps are inside the binary and are installed and upgraded at startup. An operator's own changes
# to them live on a volume mounted at /data/apps-local (SL_APPS_LOCAL_DIR), so upgrading the image never touches them.
EXPOSE 8080
ENTRYPOINT ["/servicelater"]
