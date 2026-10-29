FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ngu-emulator ./cmd/ngu-emulator \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ngu-emulator /usr/local/bin/ngu-emulator
# /data holds adoption state (auth keys); owned by the nonroot user.
COPY --from=build --chown=65532:65532 /out/data /data
VOLUME ["/data"]
EXPOSE 8098
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/usr/local/bin/ngu-emulator", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/ngu-emulator"]
CMD ["run"]
