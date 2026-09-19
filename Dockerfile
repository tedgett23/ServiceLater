# One image for every role. Build context: repo root.
FROM node:22 AS web
WORKDIR /src/web
COPY web/package*.json ./
RUN npm ci
COPY web/ ./
# adapter-static writes to ../backend/internal/webui/dist
RUN mkdir -p ../backend/internal/webui && npm run build

FROM golang:1.24 AS build
WORKDIR /src/backend
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
COPY --from=web /src/backend/internal/webui/dist ./internal/webui/dist
RUN CGO_ENABLED=0 go build -o /out/servicelater ./cmd/servicelater

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/servicelater /servicelater
EXPOSE 8080
ENTRYPOINT ["/servicelater"]
