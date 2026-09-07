#!/bin/sh
# Configures INN from scratch on every start, brings innd up, and posts the
# fixtures once. The config lives here rather than in files of its own because
# it is a dozen lines that only make sense together.
set -eu

USER_NAME=${NNTP_USER:-mock}
USER_PASS=${NNTP_PASS:-mock}
GROUP=${NNTP_GROUP:-mock.test}

cd /etc/news

# The package ships an inn.conf with a dozen required keys and no defaults for
# them, so this overrides keys in it rather than writing one
cp inn.conf.dist inn.conf
setkey() {
    if grep -qE "^[[:space:]]*#?[[:space:]]*$1:" inn.conf; then
        sed -i "s|^[[:space:]]*#\?[[:space:]]*$1:.*|$1: $2|" inn.conf
    else
        printf '%s: %s\n' "$1" "$2" >> inn.conf
    fi
}

setkey organization '"nzbStreamer test server"'
setkey domain       mock.local
setkey pathhost     news.mock.local
setkey mta          '"/bin/true %s"'
# A yenc segment is ~750 KB before nyuus overhead, close enough to the 1 MB
# default to be a trap
setkey maxartsize   33554432
# Nothing feeds this server and every article is posted by us, so an article
# older than the cutoff is not a thing that can happen
setkey artcutoff    0

cat > storage.conf <<EOF
method tradspool {
    newsgroups: *
    class: 0
}
EOF

cat > newsfeeds <<EOF
ME:*::
EOF

# Empty on purpose. A host listed here is a peer, and innd serves a peer itself
# instead of handing it to nnrpd, so listing localhost would make the poster a
# feed that never gets asked for a password
: > incoming.conf

# No default: in the auth block, so a reader has to AUTHINFO. A server that let
# an unauthenticated client through would never exercise that path
cat > readers.conf <<EOF
auth "all" {
    hosts: "*"
    auth: "ckpasswd -f /etc/news/passwd.nntp"
}

access "all" {
    users: "*"
    newsgroups: "*"
    access: RPA
}
EOF

printf '%s:%s\n' "$USER_NAME" "$(openssl passwd -6 "$USER_PASS")" > passwd.nntp

# Keep everything forever: an article expiring under a test would look exactly
# like the retention loss the damaged set simulates deliberately
cat > expire.ctl <<EOF
/remember/:0
*:A:1:never:never
EOF

chown news:news inn.conf storage.conf newsfeeds incoming.conf readers.conf passwd.nntp expire.ctl
chmod 0640 passwd.nntp

install -d -o news -g news -m 0775 \
    /var/spool/news /var/spool/news/incoming /var/spool/news/incoming/bad \
    /var/spool/news/articles /var/spool/news/overview /var/spool/news/tmp \
    /var/log/news /var/lib/news /var/run/news /nzb

if [ ! -s /var/lib/news/active ]; then
    printf '%s 0000000000 0000000001 y\n' "$GROUP" > /var/lib/news/active
    : > /var/lib/news/newsgroups
    chown news:news /var/lib/news/active /var/lib/news/newsgroups
fi

if [ ! -f /var/lib/news/history.dir ]; then
    su news -s /bin/sh -c 'cd /var/lib/news && : > history && /usr/lib/news/bin/makedbz -i -o'
fi

su news -s /bin/sh -c '/usr/lib/news/bin/innd -f' &
INND=$!

# innd is ready when it answers ctlinnd, which is sooner than it accepts a
# reader and much sooner than any fixed sleep would guess
i=0
until /usr/lib/news/bin/ctlinnd -s -t 2 mode >/dev/null 2>&1; do
    i=$((i + 1))
    if [ "$i" -gt 60 ]; then
        echo "innd did not come up" >&2
        exit 1
    fi
    sleep 1
done

/usr/lib/news/bin/ctlinnd -s newgroup "$GROUP" y mock || true

if [ ! -f /var/lib/news/.posted ]; then
    NNTP_USER="$USER_NAME" NNTP_PASS="$USER_PASS" NNTP_GROUP="$GROUP" post.sh
    touch /var/lib/news/.posted
fi

echo "news server ready: $GROUP as $USER_NAME, nzbs in /nzb"
wait "$INND"
