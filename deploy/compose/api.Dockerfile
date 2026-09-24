FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN make build dist GO=go VERSION=$(git describe --tags --always 2>/dev/null || echo docker)

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git && rm -rf /var/lib/apt/lists/*
COPY --from=build /src/bin/ /app/bin/
COPY --from=build /src/dist/ /app/dist/
ENV MLDOJO_AGENT_DIST=/app/dist MLDOJO_LISTEN=0.0.0.0:8765 MLDOJO_HOME=/data
EXPOSE 8765
ENTRYPOINT ["/app/bin/mldojo-api"]
