#!/bin/sh
# Builds one directory per shape the stack has to handle out of /payload. Each
# becomes one nzb; post.sh does not need to know what any of them contain.
set -eu

PAYLOAD=/payload
WORK=/work

[ -f "$PAYLOAD/plain.mkv" ] || { echo "no payload in $PAYLOAD; run gen first" >&2; exit 1; }

if [ -f "$WORK/.built" ]; then
    echo "fixtures already built"
    exit 0
fi

set_dir() {
    rm -rf "${WORK:?}/$1"
    mkdir -p "$WORK/$1"
    echo "building $1"
}

# Posted as it is, so this is the only set whose expected bytes are
# payload.Bytes with nothing in between
set_dir plain
cp "$PAYLOAD/plain.mkv" "$WORK/plain/"

# A second, identical copy of the plain payload, used by the
# concurrent-readers test to give two parallel readers distinct files
set_dir plain2
cp "$PAYLOAD/plain.mkv" "$WORK/plain2/"

# Stored, single volume: the common shape for a video release, and what the
# whole addressable read path exists for
set_dir rar-stored
rar a -m0 -ep -idq "$WORK/rar-stored/movie.rar" "$PAYLOAD/movie.mkv"

# Stored, multi-volume, which is what volumeFS and the volume ordering are for
set_dir rar-multi
rar a -m0 -ep -idq -v5m "$WORK/rar-multi/movie.rar" "$PAYLOAD/movie.mkv"

# Compressed, so a member that can only be read forwards is covered too
set_dir rar-compressed
rar a -m3 -ep -idq "$WORK/rar-compressed/movie.rar" "$PAYLOAD/movie.mkv"

# Solid: the whole volume is one compression stream, so a member is only
# addressable by reading forwards from the start — a different seek shape
set_dir rar-solid
rar a -ms -m3 -ep -idq "$WORK/rar-solid/movie.rar" "$PAYLOAD/movie.mkv"

set_dir 7z
7z a -bso0 -bsp0 -mx0 "$WORK/7z/movie.7z" "$PAYLOAD/movie.mkv"

# The stack does not unpack zip: this is the archive that stays an archive
set_dir zip
zip -0 -q -j "$WORK/zip/movie.zip" "$PAYLOAD/movie.mkv"

# Recovery files alongside the content, which is what filehealth measures its
# damage limit against
set_dir par2
cp "$PAYLOAD/plain.mkv" "$WORK/par2/"
( cd "$WORK/par2" && par2 create -q -r10 -n4 plain.par2 plain.mkv )

# The same content, with articles cancelled after posting; post.sh does that
# part, since only it knows the message-ids
set_dir damaged
cp "$PAYLOAD/plain.mkv" "$WORK/damaged/"

touch "$WORK/.built"
echo "built $(find "$WORK" -mindepth 1 -maxdepth 1 -type d | wc -l) sets"
