# agent-service (prod). Pure Go -> static binary -> distroless/static.
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
# go.mod replaces google.golang.org/adk with this local patched copy; its
# go.mod must exist before `go mod download` resolves the graph.
COPY third_party/adk/go.mod third_party/adk/go.sum ./third_party/adk/
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
    -o /out/agent ./services/agent/cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/agent /agent
EXPOSE 9090
ENTRYPOINT ["/agent"]
