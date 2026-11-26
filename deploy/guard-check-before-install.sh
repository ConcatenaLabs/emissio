#!/bin/bash
# Runs the new build's `emissio guard-check` on the live database, before
# anything is installed, and stops the deploy on a conflict.
#
#   guard-check-before-install.sh BINARY DATABASE USER DIR
#
# BINARY is the new build, DATABASE the live database, USER the service's
# user, DIR a directory that user can read (/opt/emissio). The build is
# copied there as emissio.check, run as USER (never as root: a read-only
# open of a WAL database can leave -wal and -shm files beside it, which must
# stay the service's), and removed again. A box with no database yet has
# nothing to check. install-on-box.sh calls it between the build and the
# install; it exits non-zero, and the install stops, when guard-check finds
# rows that would keep the new revision from starting, or cannot read the
# database.
set -euo pipefail

if [ $# -ne 4 ]; then
    echo "usage: $0 BINARY DATABASE USER DIR" >&2
    exit 2
fi
bin=$1 db=$2 user=$3 dir=$4

if [ ! -e "$db" ]; then
    echo "no database at $db yet: nothing to check"
    exit 0
fi

check="$dir/emissio.check"
install -m 755 "$bin" "$check"
trap 'rm -f "$check"' EXIT

if [ "$(id -un)" = "$user" ]; then
    as_user=()
else
    as_user=(runuser -u "$user" --)
fi
if ! ${as_user[@]+"${as_user[@]}"} env EMISSIO_DB="$db" "$check" guard-check; then
    echo "guard-check stopped the deploy: the new revision is not installed. Correct the rows it names (or the database it cannot read) and run the install again." >&2
    exit 1
fi
