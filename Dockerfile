# podswitch: the static binary and nothing else (no shell, no OS), about 8 MB.
FROM docker.io/library/golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod *.go ./
COPY web ./web
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /podswitch .

FROM scratch
COPY --from=build /podswitch /podswitch
USER 65532:65532
EXPOSE 8080
HEALTHCHECK --interval=1m --timeout=3s CMD ["/podswitch", "healthcheck"]
ENTRYPOINT ["/podswitch"]
