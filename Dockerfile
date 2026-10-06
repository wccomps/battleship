FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /battleship ./cmd/battleship

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /battleship /usr/local/bin/battleship
# battleship reads battleship.toml from the working directory; mount it here.
WORKDIR /config
ENTRYPOINT ["/usr/local/bin/battleship"]
