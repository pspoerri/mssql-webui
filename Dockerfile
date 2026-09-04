FROM node:22-alpine AS web
WORKDIR /src/web
RUN npm install -g pnpm@11
COPY web/package.json web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm build

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY --from=web /src/web/dist ./web/dist
RUN CGO_ENABLED=0 go build -o /db-webui .

FROM gcr.io/distroless/static-debian12
COPY --from=build /db-webui /db-webui
EXPOSE 8080
ENTRYPOINT ["/db-webui"]
