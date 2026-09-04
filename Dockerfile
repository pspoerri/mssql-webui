FROM docker.io/library/node:22-alpine AS web
WORKDIR /src/web
RUN npm install -g pnpm@11
COPY web/package.json web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm build

FROM docker.io/library/golang:1.27-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/*.go ./
COPY --from=web /src/backend/dist ./dist
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=$VERSION" -o /mssql-webui .

FROM gcr.io/distroless/static-debian12
COPY --from=build /mssql-webui /mssql-webui
EXPOSE 8080
ENTRYPOINT ["/mssql-webui"]
