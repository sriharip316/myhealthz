FROM docker.io/library/golang:1.26-alpine AS build
ARG version=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${version}" -o /out/myhealthz .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/myhealthz /myhealthz
EXPOSE 8080
ENTRYPOINT ["/myhealthz"]
