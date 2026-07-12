# syntax=docker/dockerfile:1

# The demo's go.mod replaces loon + loon-plugins with sibling checkouts. Until
# loon publishes tagged releases, the Docker build pulls them in via BuildKit
# named build-contexts (see docker-compose.yml -> app.build.additional_contexts):
#   --build-context loon=../loon  --build-context loonplugins=../loon-plugins
# The replace paths (../loon, ../loon-plugins) resolve to /loon, /loon-plugins
# from the /app workdir, so that's where we copy them.
FROM golang:1.26 AS build
WORKDIR /app
COPY --from=loon . /loon/
COPY --from=loonplugins . /loon-plugins/
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /loondemo .

# Static binary (CGO off); templates + static assets are embedded via embed.FS,
# so the runtime image needs nothing but the binary + CA certs (for TLS NNTP).
FROM gcr.io/distroless/static-debian12
COPY --from=build /loondemo /loondemo
EXPOSE 8090
ENTRYPOINT ["/loondemo"]
