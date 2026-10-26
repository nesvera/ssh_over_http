FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/proxy .

FROM alpine:3.20
COPY --from=build /out/proxy /usr/local/bin/proxy
USER nobody
EXPOSE 8443
ENTRYPOINT ["proxy"]
