# Static landing page. Throwaway deploy branch - not for merging.
# The platform routes <service>.<domain> to the container's port; nginx listens on 8080.
FROM nginx:alpine
COPY index.html /usr/share/nginx/html/index.html
RUN printf 'server {\n  listen 8080;\n  server_name _;\n  root /usr/share/nginx/html;\n  index index.html;\n  location / { try_files $uri $uri/ /index.html; }\n}\n' > /etc/nginx/conf.d/default.conf
EXPOSE 8080
