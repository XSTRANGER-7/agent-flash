# Static, dependency-free Go binary on a distroless base.
FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go test ./... && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/arena ./cmd/arena

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/arena /arena
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/arena"]
CMD ["serve"]
