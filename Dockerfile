# Build a static binary, then copy it into a distroless image with no shell or package
# manager, running as a non-root user.
FROM golang:1.27 AS build
WORKDIR /src
# Dependencies first, so code changes don't invalidate the module download layer.
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /workloadguard ./cmd/workloadguard

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /workloadguard /workloadguard
USER 65532:65532
ENTRYPOINT ["/workloadguard"]
