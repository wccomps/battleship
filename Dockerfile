FROM golang:1.27.2-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN want="$(sed -n 's/^toolchain //p' go.mod)"; [ -n "$want" ] || want="go$(sed -n 's/^go //p' go.mod)"; \
    case "$want" in go*.*.*) ;; *) want="$want.0" ;; esac; \
    [ "$(go env GOVERSION)" = "$want" ] || { echo "the golang image must be go.mod's $want" >&2; exit 1; }
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /battleship ./cmd/battleship

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /battleship /usr/local/bin/battleship
# battleship reads battleship.toml from the working directory; mount it here.
WORKDIR /config
ENTRYPOINT ["/usr/local/bin/battleship"]
