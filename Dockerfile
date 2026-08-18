# Static landing page. Throwaway deploy branch - not for merging.
# nginx:alpine runs envsubst on templates at start; ${PORT} follows the platform's port,
# defaulting to 8080. $uri is left alone (not an env var), so routing stays intact.
FROM nginx:alpine
ENV PORT=8080
COPY index.html /usr/share/nginx/html/index.html
COPY default.conf.template /etc/nginx/templates/default.conf.template
