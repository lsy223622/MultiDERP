FROM golang:1.26.6@sha256:0d1d3a794be25f809dd2cb3160d8c73276c4056a9f8242a138e908ddeee7b6b6 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG UNIDERP_VERSION=dev
ARG UNIDERP_COMMIT=unknown
ARG TAILSCALE_VERSION=unknown
RUN upstream_version="$(go list -m -f '{{.Version}}' tailscale.com)" && \
    upstream_version="${upstream_version#v}" && \
    patch_id="$(cat patches/tailscale/uniderp.sha256)" && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
      -ldflags "-X main.uniderpVersion=${UNIDERP_VERSION} -X main.gitCommit=${UNIDERP_COMMIT} -X main.tailscaleUpstreamVersion=${TAILSCALE_VERSION} -X main.tailscalePatchID=${patch_id} -X tailscale.com/version.longStamp=${upstream_version} -X tailscale.com/version.shortStamp=${upstream_version}" \
      -o /out/uniderp ./cmd/uniderp && \
    go run ./scripts/build-derper -goos linux -goarch amd64 -out /out/derper

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

COPY --from=build /out/uniderp /usr/local/bin/uniderp
COPY --from=build /out/derper /usr/local/bin/derper
COPY THIRD_PARTY_NOTICES.md /usr/share/licenses/uniderp/THIRD_PARTY_NOTICES.md
COPY licenses /usr/share/licenses/uniderp/licenses

USER 10001:10001
EXPOSE 80/tcp 443/tcp 3377/tcp 3478/udp
ENTRYPOINT ["/usr/local/bin/uniderp", "serve", "--config", "/data/config.yaml", "--derper", "/usr/local/bin/derper"]
