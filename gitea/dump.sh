#!/usr/bin/env bash

# A daily `gitea dump` onto the volume, keeping the newest GITEA_DUMP_KEEP.
# Wired as a cron entry of the `gitea` setup in zerops.yaml.
#
# A dump is the repositories, the database and the configuration in one
# archive: it is what a restore of this account's Gitea starts from. The
# platform's database backups cover the Postgres alone, which is why this
# exists beside them.
#
# https://docs.gitea.com/administration/command-line#dump

set -euo pipefail

cd /var/www
: "${GITEA_BIN:=/var/www/bin/gitea}"
: "${GITEA_DUMP_DIR:=/mnt/volume/dumps}"
: "${GITEA_DUMP_KEEP:=7}"
CONF=/etc/gitea/app.ini

if [ ! -f "$CONF" ]; then
  echo "dump.sh: $CONF does not exist yet, nothing to dump"
  exit 0
fi

mkdir -p "$GITEA_DUMP_DIR"
name="gitea-dump-$(date -u +%Y%m%dT%H%M%SZ).zip"

echo "dump.sh: writing $GITEA_DUMP_DIR/$name ..."
# --tempdir keeps the intermediate copy on the volume too: a dump of a real
# account does not fit in the container's own filesystem.
"$GITEA_BIN" dump --config "$CONF" \
  --file "$GITEA_DUMP_DIR/$name" \
  --tempdir "$GITEA_DUMP_DIR" \
  --skip-log

# Keep the newest GITEA_DUMP_KEEP and delete the rest. `ls -1t` is safe here:
# every name is the one this script writes, so none of them carries a newline.
count=0
for old in $(ls -1t "$GITEA_DUMP_DIR"/gitea-dump-*.zip 2>/dev/null); do
  count=$((count + 1))
  if [ "$count" -gt "$GITEA_DUMP_KEEP" ]; then
    echo "dump.sh: removing $old"
    rm -f "$old"
  fi
done

echo "dump.sh: done"
