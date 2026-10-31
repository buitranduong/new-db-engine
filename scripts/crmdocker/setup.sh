#!/bin/bash
# setup.sh: prepare ~/jdbcreal-run (outside git): verified Gradle distribution, a copy of the CRM source, a random DB password.
# No Java on the host: everything runs in eclipse-temurin (pinned). No secret or customer data is written under the repo.
set -euo pipefail
R=${RUN:-$HOME/jdbcreal-run}; SRC=${CRM_SRC:-$HOME/crm}
GRADLE_VER=8.14.4
GRADLE_SHA=f1771298a70f6db5a29daf62378c4e18a17fc33c9ba6b14362e0cdf40610380d   # = https://services.gradle.org/distributions/gradle-$GRADLE_VER-bin.zip.sha256
IMG=eclipse-temurin:17.0.13_11-jdk                                              # digest sha256:0613a19436dc8f745914b25235d43f3b0eddb8d432d19edce30ffaf2d2f95403
mkdir -p "$R/dist" "$R/gradle-home"; chmod 700 "$R"
if [ ! -f "$R/dist/gradle-$GRADLE_VER-bin.zip" ]; then curl -fsSL -o "$R/dist/gradle-$GRADLE_VER-bin.zip" "https://services.gradle.org/distributions/gradle-$GRADLE_VER-bin.zip"; fi
echo "$GRADLE_SHA  $R/dist/gradle-$GRADLE_VER-bin.zip" | sha256sum -c -
docker pull "$IMG" >/dev/null
rsync -a --delete --exclude node_modules --exclude build --exclude .gradle --exclude web "$SRC/" "$R/crm/"
# the wrapper reads the verified local zip and enforces its checksum
cat > "$R/crm/gradle/wrapper/gradle-wrapper.properties" <<EOP
distributionBase=GRADLE_USER_HOME
distributionPath=wrapper/dists
distributionUrl=file\:///dist/gradle-$GRADLE_VER-bin.zip
distributionSha256Sum=$GRADLE_SHA
zipStoreBase=GRADLE_USER_HOME
zipStorePath=wrapper/dists
EOP
if [ ! -f "$R/env.sh" ]; then
  umask 077; { echo "export CRM_DB_USER=app"; echo "export CRM_DB_PASSWORD=$(head -c 24 /dev/urandom | base64 | tr -dc A-Za-z0-9)"; } > "$R/env.sh"
fi
echo ok
