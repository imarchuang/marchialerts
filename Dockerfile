FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /bin/marchialerts ./cmd/marchialerts

FROM alpine:3.20
RUN adduser -D -u 10001 marchialerts
USER marchialerts
COPY --from=build /bin/marchialerts /bin/marchialerts
COPY config.example.yaml /etc/marchialerts/config.yaml
EXPOSE 9094
ENTRYPOINT ["/bin/marchialerts", "-config", "/etc/marchialerts/config.yaml"]
