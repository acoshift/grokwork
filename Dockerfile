FROM golang:1.26.5 AS build

ENV CGO_ENABLED=0
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/grokwork .

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /
COPY --from=build /out/grokwork /grokwork

# Mount at runtime:
#   - /config/config.json  (or set GROK_WORK_CONFIG)
#   - project trees under paths listed in config
#   - grok binary on PATH, or set grokBin to an absolute mounted path
#   - ~/.grok or XAI_API_KEY for auth
ENV GROK_WORK_CONFIG=/config/config.json

USER nonroot:nonroot
ENTRYPOINT ["/grokwork"]
