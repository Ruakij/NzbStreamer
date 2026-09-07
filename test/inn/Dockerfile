# INN2 and a third-party poster. Everything this puts on the wire - the yenc
# encoding, the subject lines, the nzb, the bytes hints - is produced by
# something that is not this project, which is the point: our own encoder would
# agree with our own decoder about a bug they share.
FROM debian:bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive

# The postinst runs innconfval, which refuses a build sandbox whose hostname
# does not resolve and whose inn.conf names no domain. /etc/hosts is read-only
# here, so the domain is the half that can be fixed - which means letting the
# configure step fail, setting it in the file the unpack just placed, and
# configuring again. The pristine file is kept for the entrypoint to override
# keys in, since the package ships a dozen that are required and have no default.
RUN apt-get update \
 && { apt-get install -y --no-install-recommends inn2 openssl procps || true; } \
 && sed -i 's/^#*[[:space:]]*domain:.*/domain: mock.local/' /etc/news/inn.conf \
 && grep -q '^domain:' /etc/news/inn.conf || echo 'domain: mock.local' >> /etc/news/inn.conf \
 && dpkg --configure -a \
 && cp /etc/news/inn.conf /etc/news/inn.conf.dist \
 && rm -rf /var/lib/apt/lists/*

# nyuu builds its yencode module from source, so the toolchain has to be here
# for the install and nowhere near the running server
RUN apt-get update \
 && apt-get install -y --no-install-recommends nodejs npm build-essential python3 \
 && npm install -g --no-fund --no-audit nyuu \
 && apt-get purge -y npm build-essential python3 && apt-get autoremove -y \
 && rm -rf /var/lib/apt/lists/* /root/.npm /root/.cache

COPY entrypoint.sh post.sh /usr/local/bin/
RUN chmod 0755 /usr/local/bin/entrypoint.sh /usr/local/bin/post.sh

EXPOSE 119
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
