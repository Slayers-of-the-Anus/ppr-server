FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/phonepilot-server ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/phonepilot-server /app/phonepilot-server
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/app/phonepilot-server"]
