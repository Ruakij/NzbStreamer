#!/bin/sh
# Posts every fixture set in /work as its own nzb. Run once, by the entrypoint,
# when /nzb has nothing in it.
set -eu

USER_NAME=${NNTP_USER:-mock}
USER_PASS=${NNTP_PASS:-mock}
GROUP=${NNTP_GROUP:-mock.test}
SEGMENT=${NNTP_SEGMENT_SIZE:-768000}

WORK=/work
OUT=/nzb

[ -f "$WORK/.built" ] || { echo "no fixtures in $WORK" >&2; exit 1; }

for dir in "$WORK"/*/; do
    name=$(basename "$dir")
    # The sequential lifecycle posts one set at a time; an empty FIXTURE_SETS
    # posts everything
    if [ -n "${FIXTURE_SETS:-}" ]; then
        case ",$FIXTURE_SETS," in
            *",$name,"*) ;;
            *) continue ;;
        esac
    fi
    echo "posting $name"
    ( cd "$dir" && nyuu \
        --host 127.0.0.1 --port 119 \
        --user "$USER_NAME" --password "$USER_PASS" \
        --connections 4 \
        --groups "$GROUP" \
        --from "mock <mock@mock.local>" \
        --article-size "$SEGMENT" \
        --nzb-title "$name" \
        --overwrite --quiet \
        --out "$OUT/$name.nzb" \
        ./* )
done

# Cancelling is how an article goes missing on a real server, and what an
# aged-out post looks like to a client. Every second article, so the set is
# unambiguously beyond repair: a handful of holes would need more samples than
# PROBE_INITIAL_FILE_PERCENT takes to be found at all, which is a property of
# sampling rather than something a fixture should be tuned around. The first
# article is among them, and sampleIndices always includes the first, so the
# check fails deterministically rather than most of the time.
for id in $(sed -n 's|.*<segment[^>]*>\(.*\)</segment>.*|\1|p' "$OUT/damaged.nzb" | awk 'NR % 2 == 1'); do
    echo "cancelling <$id>"
    su news -s /bin/sh -c "/usr/lib/news/bin/ctlinnd -s cancel '<$id>'"
done

echo "posted $(find "$OUT" -name '*.nzb' | wc -l) nzbs"
