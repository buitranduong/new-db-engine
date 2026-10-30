# usage: boot.sh stask|pg  (logs /tmp/crm-run/app-$1.log)
. /tmp/crm-run/env.sh
export JAVA_HOME=$HOME/rc-tools/jdk PATH=$HOME/rc-tools/jdk/bin:$PATH
T=$1; if [ $T = pg ]; then PORT=28601; else PORT=28602; fi
cd ~/crm
nohup setsid ./gradlew --no-daemon bootRun --args="--main.datasource.url=jdbc:postgresql://127.0.0.1:$PORT/crm --main.liquibase.change-log=com/dth/crm/liquibase/${CL:-changelog-crmcompat.xml} --server.port=${2:-28600}" </dev/null >/tmp/crm-run/app-$T.log 2>&1 &
