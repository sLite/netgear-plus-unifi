FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/netgear-unifi ./cmd/netgear-unifi \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/netgear-unifi /usr/local/bin/netgear-unifi
# /data holds adoption state (auth keys); owned by the nonroot user.
COPY --from=build --chown=65532:65532 /out/data /data
VOLUME ["/data"]
EXPOSE 8098
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/usr/local/bin/netgear-unifi", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/netgear-unifi"]
CMD ["run"]
