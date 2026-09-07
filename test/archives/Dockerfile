# Builds the fixture sets from the generated payloads. Its own image, and its
# own architecture: RARLAB ships no linux/arm64 rar, and emulating one one-shot
# archiver is cheaper than emulating the news server everything is timed
# against.
FROM --platform=linux/amd64 debian:bookworm-slim

ARG RAR_URL=https://www.rarlab.com/rar/rarlinux-x64-723.tar.gz

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
 && apt-get install -y --no-install-recommends p7zip-full zip par2 curl ca-certificates \
 && curl -fsSL "$RAR_URL" -o /tmp/rar.tar.gz \
 && tar -xzf /tmp/rar.tar.gz -C /tmp \
 && install -m 0755 /tmp/rar/rar /usr/local/bin/rar \
 && rm -rf /tmp/rar* /var/lib/apt/lists/*

COPY archives.sh /usr/local/bin/
RUN chmod 0755 /usr/local/bin/archives.sh

ENTRYPOINT ["/usr/local/bin/archives.sh"]
