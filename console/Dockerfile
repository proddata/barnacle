FROM node:22-alpine AS build
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci
COPY index.html vite.config.js ./
COPY src ./src
RUN npm run build

FROM nginx:stable-alpine
COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/dist/ /usr/share/nginx/html/
COPY --from=build /app/node_modules/@neondatabase/serverless/LICENSE /usr/share/doc/barnacle-console/neon-LICENSE
COPY --from=build /app/node_modules/monaco-editor/LICENSE /usr/share/doc/barnacle-console/monaco-LICENSE
EXPOSE 80
