# The three backend URLs are baked in at BUILD time (Vite env vars resolve
# into the static bundle, not at container runtime), so they must match
# wherever the browser itself -- not other containers -- can reach those
# services. In ledgerline-docs' docker-compose.yml, gateway/balance-reader/fx
# are published to the host on the same ports as their own defaults, so the
# plain defaults baked in here (see src/config.ts) work unmodified.
FROM node:22-alpine AS build
WORKDIR /src
COPY package*.json ./
RUN npm install
COPY . .
RUN npm run build

FROM nginx:1.27-alpine
COPY --from=build /src/dist /usr/share/nginx/html
EXPOSE 80
