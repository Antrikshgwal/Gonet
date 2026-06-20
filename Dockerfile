# --- build stage -------------------------------------------------------------
FROM golang:1.26-alpine AS build
WORKDIR /src

# Cache deps first: only re-download when go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Static binary so it runs in a minimal final image.
RUN CGO_ENABLED=0 go build -o /bin/server ./cmd/server

# --- run stage ---------------------------------------------------------------
FROM alpine:3.20
WORKDIR /app
COPY --from=build /bin/server /app/server
COPY client ./client

ENV ADDR=:8080
ENV STATIC_DIR=/app/client
EXPOSE 8080
ENTRYPOINT ["/app/server"]
