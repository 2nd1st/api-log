FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0
RUN go build -ldflags="-s -w" -o /out/mockup ./tools/mockup

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mockup /usr/local/bin/mockup
USER nonroot:nonroot
EXPOSE 8888
ENTRYPOINT ["/usr/local/bin/mockup", "-listen", ":8888"]
