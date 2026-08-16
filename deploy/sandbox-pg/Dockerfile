# A Postgres you can reach with psql, on a platform that only speaks HTTP.
#
# The platform gives one container and one HTTP port, and a database speaks TCP - so the two
# do not meet, and that is the whole reason this image exists. Postgres listens on loopback
# where nothing outside the container can reach it; sbx serves the connect endpoint on the one
# port the platform *does* route, and carries TCP to it over a WebSocket. `sbx connect` on a
# laptop then presents an ordinary local 5432 and psql connects to it knowing nothing.
#
# What you get from the platform for free: HTTPS, a hostname, and scale-to-zero - the database
# costs nothing while nobody is connected, which is the same bargain sbx makes locally.
#
# What you do NOT get, and should know before storing anything you care about: a zopcloud
# service has no persistent volume, so this is a database that starts empty every time the
# container is replaced. It is the right shape for a branch's test data and the wrong shape for
# anything you would miss.

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/sbx .

FROM postgres:16-alpine
COPY --from=build /out/sbx /usr/local/bin/sbx
COPY deploy/sandbox-pg/start.sh /start.sh

# No default password. It would be baked into the image, and an image is not a secret - the
# platform passes this in as an environment variable at deploy time, where it belongs.
ENV PORT=8080 \
    POSTGRES_DB=sbx \
    PGDATA=/var/lib/postgresql/data

EXPOSE 8080
ENTRYPOINT ["/start.sh"]
