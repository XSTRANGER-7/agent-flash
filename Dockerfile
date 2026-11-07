# Static, dependency-free Go binary on a distroless base.
# Secrets are NOT baked in: pass the env vars (FLASH_API_KEY, ADMIN_TOKEN, ...)
# at run time and mount wallets.json read-only. See README "Deploy".
FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod ./
COPY . .
# Tests gate every build; deploy.sh --skip-tests passes RUN_TESTS=0 for a hotfix.
ARG RUN_TESTS=1
RUN if [ "$RUN_TESTS" = "1" ]; then CGO_ENABLED=0 go test ./...; fi && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/arena ./cmd/arena
# A writable data folder for the nonroot user (distroless has no shell to mkdir).
RUN mkdir -p /out/data /out/secrets

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/arena /app/arena
COPY --from=build --chown=nonroot:nonroot /out/data /app/data
COPY --from=build --chown=nonroot:nonroot /out/secrets /app/secrets
ENV HTTP_ADDR=:8080 \
    DATA_DIR=/app/data \
    WALLETS_FILE=/app/secrets/wallets.json \
    MODE=paper
USER nonroot:nonroot
EXPOSE 8080
VOLUME ["/app/data"]
ENTRYPOINT ["/app/arena"]
CMD ["serve"]
