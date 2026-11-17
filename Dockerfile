# One image for every role. Build context: repo root.
FROM node:22 AS web
WORKDIR /src/web
COPY web/package*.json ./
RUN npm ci
COPY web/ ./
# adapter-static writes to ../backend/internal/webui/dist
RUN mkdir -p ../backend/internal/webui && npm run build

# Keep this in step with the go line of backend/go.mod: the image's Go does not download a newer toolchain.
FROM golang:1.26 AS build
# The first-party apps are a Go module of their own (they embed themselves), which the backend module points at.
COPY apps /src/apps
WORKDIR /src/backend
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
COPY --from=web /src/backend/internal/webui/dist ./internal/webui/dist
# The source revision, for `servicelater version` (the build context has no .git): --build-arg COMMIT=<sha>.
ARG COMMIT=""
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-X main.commit=${COMMIT}" -o /out/servicelater ./cmd/servicelater
# The folders volumes are mounted on, owned by the user the server runs as, so a new named volume starts writable.
RUN mkdir -p /out/data/attachments /out/data/apps-local /out/data/certs

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/servicelater /servicelater
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
# The first-party apps are inside the binary and are installed and upgraded at startup. An operator's own changes
# to them live on a volume mounted at /data/apps-local (SL_APPS_LOCAL_DIR), so upgrading the image never touches them.
EXPOSE 8080
ENTRYPOINT ["/servicelater"]
