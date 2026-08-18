# Static landing page. Throwaway deploy branch - not for merging.
# The platform routes <service>.<domain> to the container's port; nginx listens on 8080.
FROM nginx:alpine
COPY index.html /usr/share/nginx/html/index.html
RUN sed -i 's/listen       80;/listen       8080;/' /etc/nginx/conf.d/default.conf
EXPOSE 8080
