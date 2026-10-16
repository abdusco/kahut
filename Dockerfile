FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /kahut .

FROM alpine:3.23
RUN addgroup -S kahut && adduser -S -G kahut kahut && mkdir /data && chown kahut:kahut /data
COPY --from=build /kahut /usr/local/bin/kahut
USER kahut
ENV DB_PATH=/data/kahut.db PORT=8080
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["kahut"]
