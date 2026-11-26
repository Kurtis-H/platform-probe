# Static binary on a distroless image: no shell, no package manager, non-root.
FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /platform-probe ./cmd/platform-probe

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /platform-probe /platform-probe
COPY examples/booking.json /etc/platform-probe/probes.json
EXPOSE 9100
ENTRYPOINT ["/platform-probe"]
CMD ["serve", "-config", "/etc/platform-probe/probes.json", "-listen", ":9100"]
