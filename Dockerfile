FROM golang:1.26.10-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN want="go$(sed -n 's/^go //p' go.mod)"; [ "$(go env GOVERSION)" = "$want" ] || { echo "the golang image must be go.mod's $want" >&2; exit 1; }
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /battleship ./cmd/battleship

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /battleship /usr/local/bin/battleship
# battleship reads battleship.toml from the working directory; mount it here.
WORKDIR /config
ENTRYPOINT ["/usr/local/bin/battleship"]
