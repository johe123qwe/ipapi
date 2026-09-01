#!/usr/bin/env bash
# Downloads the source databases into data/src.
#
#   geolocation  ipapi.is free tier (updated several times per week)
#   ip2asn       iptoasn.com, derived from RouteViews (updated hourly)
set -euo pipefail

SRC="${SRC:-data/src}"
REPO_RAW="https://raw.githubusercontent.com/ipapi-is/ipapi/main/databases"
mkdir -p "$SRC"

fetch() { # url dest
  echo ">> $2"
  curl -fL --retry 3 --retry-delay 2 --progress-bar -o "$2.part" "$1"
  mv "$2.part" "$2"
}

fetch "$REPO_RAW/geolocationDatabaseIPv4.csv.zip" "$SRC/geolocationDatabaseIPv4.csv.zip"
fetch "$REPO_RAW/geolocationDatabaseIPv6.csv.zip" "$SRC/geolocationDatabaseIPv6.csv.zip"
fetch "https://iptoasn.com/data/ip2asn-combined.tsv.gz" "$SRC/ip2asn-combined.tsv.gz"

echo ">> extracting"
unzip -o -q -j "$SRC/geolocationDatabaseIPv4.csv.zip" -d "$SRC"
unzip -o -q -j "$SRC/geolocationDatabaseIPv6.csv.zip" -d "$SRC"
gzip -dkf "$SRC/ip2asn-combined.tsv.gz"

ls -lh "$SRC" | grep -E '\.csv|\.tsv' || true
