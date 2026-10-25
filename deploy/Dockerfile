# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
ARG SERVICE
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./services/${SERVICE}/cmd

FROM gcr.io/distroless/static-debian12:nonroot
# RDS CA bundle for sslmode=verify-full in AWS (see pkg/config).
ADD --chmod=444 https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem /etc/ssl/rds/global-bundle.pem
COPY --from=build /out/app /app
USER nonroot
ENTRYPOINT ["/app"]
