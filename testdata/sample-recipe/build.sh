#!/bin/sh
# Sample build script. peipkg-manager invokes this with:
#   SOURCE_DIR — checked-out upstream source tree (no .git/)
#   DESTDIR    — fresh empty staging directory
#   SOURCE_DATE_EPOCH — UNIX seconds equivalent of the build timestamp
#   LC_ALL=C TZ=UTC
#
# A real build would `./configure --prefix=/usr && make && make install
# DESTDIR=$DESTDIR`. This sample is a placeholder.
set -eu
cd "$SOURCE_DIR"

# ./configure --prefix=/usr --libdir=/usr/lib/x86_64-linux-peios
# make
# make install DESTDIR="$DESTDIR"

echo "build.sh: real build steps go here" >&2
exit 1
