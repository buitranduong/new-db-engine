#!/bin/bash
# crm-up.sh [build|boot]: run the real Jmix CRM in a pinned JDK 17 container against STASK (host networking).
# env: STASK_PG_PORT (default 28810) APP_PORT (28812) CL (changelog, default changelog-crmcompat.xml) NAME (container)
set -euo pipefail
R=${RUN:-$HOME/jdbcreal-run}; . "$R/env.sh"
IMG=eclipse-temurin:17.0.13_11-jdk; NAME=${NAME:-jdbcreal-crm}
docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" --network host -m 6g --user "$(id -u):$(id -g)" -e HOME=/tmp \
  -e GRADLE_USER_HOME=/gh -e CRM_DB_USER -e CRM_DB_PASSWORD \
  -v "$R/crm:/work" -v "$R/gradle-home:/gh" -v "$R/dist:/dist:ro" -w /work "$IMG" \
  ./gradlew --no-daemon bootRun --args="--main.datasource.url=jdbc:postgresql://127.0.0.1:${STASK_PG_PORT:-28810}/crm --main.liquibase.change-log=com/dth/crm/liquibase/${CL:-changelog-crmcompat.xml} --server.port=${APP_PORT:-28812}"
