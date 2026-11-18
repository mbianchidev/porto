#!/usr/bin/env bash
# Exercises the Docker CLI, Buildx and Compose against an installed Porto engine through the
# current Docker context (normally "porto") and reports PASS/FAIL per command. It pulls public
# images, starts a disposable local registry on 127.0.0.1:5055-5056, labels everything it creates
# with porto.sweep=1 and removes it on exit. Swarm and engine plugins are out of scope.
#
# usage: scripts/docker-cli-sweep.sh        (DOCKER=/path/to/docker, SWEEP_TIMEOUT=seconds, SWEEP_LOG=file)
set -u
D=${DOCKER:-docker}
L=porto.sweep=1
WORK=$(mktemp -d /tmp/porto-sweep.XXXXXX)
LOG=${SWEEP_LOG:-/tmp/porto-docker-cli-sweep.log}
: >"$LOG"
PASS=0; FAIL=0; FAILED=()
IMG=alpine:3.20

set -m
tty_run() {
  if script --version >/dev/null 2>&1; then
    script -q -e -c "$(printf '%q ' "$@")" /dev/null
  else
    script -q /dev/null "$@"
  fi
}
export -f tty_run
t() {
  local name=$1; shift
  local out tmp=$(mktemp) pid watchdog rc
  "$@" >"$tmp" 2>&1 </dev/null &
  pid=$!
  ( sleep "${SWEEP_TIMEOUT:-180}"; echo "TIMEOUT after ${SWEEP_TIMEOUT:-180}s" >>"$tmp"; kill -KILL -"$pid" 2>/dev/null ) &
  watchdog=$!
  wait "$pid"; rc=$?
  kill -KILL -"$watchdog" 2>/dev/null; wait "$watchdog" 2>/dev/null
  out=$(cat "$tmp"); rm -f "$tmp"
  if [ "$rc" -eq 0 ]; then
    PASS=$((PASS+1)); printf 'PASS  %s\n' "$name"
  else
    FAIL=$((FAIL+1)); FAILED+=("$name"); printf 'FAIL  %s\n' "$name"
    printf '%s\n' "$out" | tail -5 | sed 's/^/      /'
  fi
  printf '=== %s\n%s\n' "$name" "$out" >>"$LOG"
}
sh_t() { local name=$1; shift; t "$name" bash -c "$*"; }

cleanup() {
  $D rm -f $($D ps -aq --filter label=$L) >/dev/null 2>&1
  $D compose -p porto-sweep -f "$WORK/compose/compose.yaml" down -v --remove-orphans >/dev/null 2>&1
  $D network rm sweep-net >/dev/null 2>&1
  $D volume rm sweep-vol >/dev/null 2>&1
  $D rmi -f sweep/app:1 sweep/app:nocache sweep/buildx:1 sweep/commit:1 sweep/imported:1 sweep/tagged:1 \
    localhost:5055/sweep/alpine:1 localhost:5056/sweep/alpine:1 sweep/loaded:1 porto-sweep-web >/dev/null 2>&1
  $D logout localhost:5056 >/dev/null 2>&1
}
trap 'cleanup; rm -rf "$WORK"' EXIT
cleanup

echo "docker: $(command -v $D) -> $(readlink "$(command -v $D)" 2>/dev/null)"
echo "context: $($D context show)  log: $LOG"

# System
t "version"                $D version
t "info"                   $D info --format '{{.ServerVersion}} {{.OperatingSystem}}'
t "context ls"             $D context ls
t "context inspect porto"  $D context inspect porto
t "system df"              $D system df
t "system df -v"           $D system df -v
t "events (bounded)"       $D events --since 5m --until 0s
t "canonical socket info"  $D --host unix:///var/run/docker.sock info --format '{{.ServerVersion}}'
t "default context info"   $D --context default info --format '{{.ServerVersion}}'

# Images
t "pull"                   $D pull -q $IMG
t "images"                 $D images
t "image ls --filter"      $D image ls --filter reference=alpine
t "image inspect"          $D image inspect $IMG --format '{{.Id}}'
t "history"                $D history $IMG
t "tag"                    $D tag $IMG sweep/tagged:1
t "rmi tag"                $D rmi sweep/tagged:1
t "search"                 $D search --limit 2 alpine
t "manifest inspect"       $D manifest inspect $IMG
t "save"                   $D save -o "$WORK/alpine.tar" $IMG
sh_t "load"                "$D tag $IMG sweep/loaded:1 && $D save sweep/loaded:1 -o '$WORK/loaded.tar' && $D rmi sweep/loaded:1 && $D load -i '$WORK/loaded.tar' && $D image inspect sweep/loaded:1 >/dev/null"
t "image prune (label)"    $D image prune -f --filter label=$L

# Containers
t "run --rm"               $D run --rm --label $L $IMG echo hello
t "run -i stdin"           bash -c "echo piped | $D run --rm -i --label $L $IMG cat | grep -q piped"
t "start -ai stdin"        bash -c "$D create -i --name sweep-ai --label $L $IMG cat >/dev/null && echo piped-ai | $D start -ai sweep-ai | grep -q piped-ai"
t "run -d -i stays running"  bash -c "$D run -d -i --name sweep-di --label $L $IMG sh >/dev/null && sleep 2 && $D inspect sweep-di --format '{{.State.Status}}' | grep -qx running"
t "exec into -d -i"         $D exec sweep-di echo exec-ok
t "attach input to -d -i"   bash -c "$D run -d -i --name sweep-dic --label $L $IMG sh -c 'read line; echo got-\$line' >/dev/null && sleep 2 && echo hello | $D attach sweep-dic | grep -q got-hello"
t "attach -dit (tty)"       bash -c "$D run -dit --name sweep-tty --label $L $IMG sh >/dev/null && sleep 1 && printf 'echo tty-ok\nexit\n' | tty_run $D attach sweep-tty | grep -q tty-ok"
t "run -d"                 $D run -d --name sweep-c1 --label $L $IMG sh -c 'echo started; sleep 600'
t "ps"                     $D ps
t "ps -a --filter"         $D ps -a --filter label=$L
t "ps --format"            $D ps --format '{{.Names}} {{.Status}}'
t "logs"                   bash -c "$D logs sweep-c1 | grep -q started"
t "logs --tail -t"         $D logs --tail 1 -t sweep-c1
t "exec"                   $D exec sweep-c1 uname -a
t "exec -i"                bash -c "echo x | $D exec -i sweep-c1 cat"
t "exec -e -w -u"          $D exec -e FOO=bar -w /tmp -u nobody sweep-c1 sh -c 'echo $FOO; pwd; id -un'
t "exec -it (script tty)"  tty_run $D exec -it sweep-c1 sh -c 'tty'
t "top"                    $D top sweep-c1
t "stats --no-stream"      $D stats --no-stream sweep-c1
t "inspect"                $D inspect sweep-c1 --format '{{.State.Status}}'
t "container inspect"      $D container inspect sweep-c1
t "cp to"                  bash -c "echo payload > '$WORK/in.txt' && $D cp '$WORK/in.txt' sweep-c1:/tmp/in.txt"
t "cp from"                bash -c "$D cp sweep-c1:/tmp/in.txt '$WORK/out.txt' && grep -q payload '$WORK/out.txt'"
t "diff"                   $D diff sweep-c1
t "pause"                  $D pause sweep-c1
t "unpause"                $D unpause sweep-c1
t "update"                 $D update --memory 256m --memory-swap 512m --cpus 1 sweep-c1
t "rename"                 $D rename sweep-c1 sweep-c2
t "restart"                $D restart -t 1 sweep-c2
t "stop"                   $D stop -t 1 sweep-c2
t "start"                  $D start sweep-c2
t "commit"                 $D commit sweep-c2 sweep/commit:1
t "export"                 $D export -o "$WORK/export.tar" sweep-c2
t "import"                 $D import "$WORK/export.tar" sweep/imported:1
t "kill"                   $D kill sweep-c2
t "wait"                   $D wait sweep-c2
t "create + start -a"      bash -c "$D create --name sweep-c3 --label $L $IMG echo created >/dev/null && $D start -a sweep-c3 | grep -q created"
t "attach"                 bash -c "$D run -d --name sweep-c4 --label $L $IMG sh -c 'sleep 2; echo attached' >/dev/null && $D attach sweep-c4 | grep -q attached"
t "run -t"                 tty_run $D run --rm -t --label $L $IMG tty
t "run env/workdir/user"   $D run --rm --label $L -e A=1 -w /tmp -u 65534 $IMG sh -c 'test "$A" = 1 && pwd && id -u'
t "run --restart"          $D run -d --name sweep-r --label $L --restart unless-stopped $IMG sleep 600
t "run limits"             $D run --rm --label $L --memory 128m --cpus 0.5 --pids-limit 64 $IMG true
t "run --read-only tmpfs"  $D run --rm --label $L --read-only --tmpfs /tmp $IMG sh -c 'touch /tmp/ok'
t "run --cap-add"          $D run --rm --label $L --cap-add NET_ADMIN $IMG ip link
t "run --add-host --dns"   $D run --rm --label $L --add-host sweep.local:10.1.2.3 --dns 1.1.1.1 $IMG sh -c 'grep -q sweep.local /etc/hosts'
t "run healthcheck"        $D run -d --name sweep-h --label $L --health-cmd 'true' --health-interval 1s $IMG sleep 600
t "run bind mount"         bash -c "mkdir -p '$WORK/bind' && echo bound > '$WORK/bind/f' && $D run --rm --label $L -v '$WORK/bind:/b' $IMG cat /b/f | grep -q bound"
t "run bind write-back"    bash -c "$D run --rm --label $L -v '$WORK/bind:/b' $IMG sh -c 'echo back > /b/g' && grep -q back '$WORK/bind/g'"
t "run publish + curl"     bash -c "$D run -d --name sweep-web --label $L -p 127.0.0.1:0:80 nginx:alpine >/dev/null && sleep 3 && p=\$($D port sweep-web 80 | head -1 | sed 's/.*://') && curl -fsS http://127.0.0.1:\$p/ | grep -qi nginx"
t "port"                   $D port sweep-web
t "run -P"                 $D run -d --name sweep-P --label $L -P nginx:alpine
t "run platform amd64"     $D run --rm --label $L --platform linux/amd64 $IMG uname -m
t "container ls -s"        $D container ls -s
t "container prune (label)" $D container prune -f --filter label=$L

# Volumes
t "volume create"          $D volume create --label $L sweep-vol
t "volume ls"              $D volume ls --filter label=$L
t "volume inspect"         $D volume inspect sweep-vol
t "volume use"             bash -c "$D run --rm --label $L -v sweep-vol:/data $IMG sh -c 'echo v > /data/v' && $D run --rm --label $L -v sweep-vol:/data:ro $IMG cat /data/v | grep -q v"
t "volume prune (label)"   $D volume prune -f --filter label=nonexistent.sweep=1

# Networks
t "network create"         $D network create --label $L sweep-net
t "network ls"             $D network ls
t "network inspect"        $D network inspect sweep-net
t "network DNS by name"    bash -c "$D run -d --name sweep-n1 --label $L --network sweep-net $IMG sleep 600 >/dev/null && $D run --rm --label $L --network sweep-net $IMG ping -c1 -W3 sweep-n1"
t "network connect"        $D network connect sweep-net sweep-r
t "network disconnect"     $D network disconnect sweep-net sweep-r
t "network prune (label)"  $D network prune -f --filter label=nonexistent.sweep=1
t "outbound internet"      $D run --rm --label $L $IMG wget -q -O /dev/null https://example.com

# Build
mkdir -p "$WORK/build"
cat >"$WORK/build/Dockerfile" <<'EOF'
FROM alpine:3.20 AS base
ARG GREETING=hi
RUN echo "$GREETING" > /greeting
FROM base
RUN --mount=type=secret,id=token sh -c 'test -f /run/secrets/token'
COPY file.txt /file.txt
LABEL porto.sweep=1
CMD ["cat", "/greeting"]
EOF
echo file >"$WORK/build/file.txt"; echo secret >"$WORK/token"
t "build"                  $D build -q --secret id=token,src="$WORK/token" --build-arg GREETING=yo -t sweep/app:1 "$WORK/build"
t "build --no-cache"       $D build -q --no-cache --secret id=token,src="$WORK/token" -t sweep/app:nocache "$WORK/build"
t "build output check"     bash -c "$D run --rm --label $L sweep/app:1 | grep -q yo"
t "build stdin Dockerfile" bash -c "printf 'FROM $IMG\nRUN true\n' | $D build -q -f - '$WORK/build'"
t "buildx version"         $D buildx version
t "buildx ls"              $D buildx ls
t "buildx inspect"         $D buildx inspect
t "buildx build --load"    $D buildx build -q --load --secret id=token,src="$WORK/token" -t sweep/buildx:1 "$WORK/build"
t "buildx build amd64"     $D buildx build -q --platform linux/amd64 --secret id=token,src="$WORK/token" "$WORK/build"
t "buildx du"              $D buildx du
t "builder ls"             $D builder ls
t "image ls (built)"       $D image ls sweep/app

# Registry push/pull (anonymous)
t "registry run"           $D run -d --name sweep-reg --label $L -p 127.0.0.1:5055:5000 registry:2
sleep 2
t "push"                   bash -c "$D tag $IMG localhost:5055/sweep/alpine:1 && $D push -q localhost:5055/sweep/alpine:1"
t "pull from registry"     bash -c "$D rmi localhost:5055/sweep/alpine:1 >/dev/null && $D pull -q localhost:5055/sweep/alpine:1"

# Registry login/logout (htpasswd; exercises the credential store)
mkdir -p "$WORK/auth"
t "htpasswd"               bash -c "$D run --rm --label $L --entrypoint htpasswd httpd:2-alpine -Bbn sweep sweeppass > '$WORK/auth/htpasswd'"
t "auth registry run"      $D run -d --name sweep-reg-auth --label $L -p 127.0.0.1:5056:5000 -v "$WORK/auth:/auth" \
  -e REGISTRY_AUTH=htpasswd -e REGISTRY_AUTH_HTPASSWD_REALM=sweep -e REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd registry:2
sleep 2
t "login"                  bash -c "echo sweeppass | $D login localhost:5056 -u sweep --password-stdin"
t "authenticated push"     bash -c "$D tag $IMG localhost:5056/sweep/alpine:1 && $D push -q localhost:5056/sweep/alpine:1"
t "logout"                 $D logout localhost:5056

# Compose
mkdir -p "$WORK/compose/web"
cat >"$WORK/compose/web/Dockerfile" <<'EOF'
FROM nginx:alpine
RUN echo compose-ok > /usr/share/nginx/html/index.html
EOF
cat >"$WORK/compose/compose.yaml" <<'EOF'
name: porto-sweep
services:
  web:
    build: ./web
    image: porto-sweep-web
    ports: ["127.0.0.1:0:80"]
    labels: ["porto.sweep=1"]
    depends_on:
      db: { condition: service_healthy }
    volumes: ["./web:/srv/web:ro"]
  db:
    image: postgres:16-alpine
    environment: { POSTGRES_PASSWORD: sweep }
    labels: ["porto.sweep=1"]
    healthcheck: { test: ["CMD", "pg_isready", "-U", "postgres"], interval: 2s, retries: 30 }
    volumes: ["dbdata:/var/lib/postgresql/data"]
volumes:
  dbdata: {}
EOF
C="$D compose -p porto-sweep -f $WORK/compose/compose.yaml"
t "compose version"        $D compose version
t "compose config"         $C config -q
t "compose build --no-cache" $C build --no-cache
t "compose pull"           $C pull db
t "compose up -d --wait"   $C up -d --wait
t "compose ps"             $C ps
t "compose ls"             $D compose ls
t "compose logs"           $C logs --tail 5
t "compose exec"           $C exec -T db psql -U postgres -c 'select 1'
t "compose run --rm"       $C run --rm --no-deps -T web cat /usr/share/nginx/html/index.html
t "compose run -T stdin"   bash -c "echo piped-compose | $C run --rm --no-deps -T web cat | grep -q piped-compose"
t "compose port + curl"    bash -c "p=\$($C port web 80 | sed 's/.*://') && curl -fsS http://127.0.0.1:\$p/ | grep -q compose-ok"
t "compose top"            $C top
t "compose images"         $C images
t "compose restart"        $C restart web
t "compose stop"           $C stop
t "compose start"          $C start
t "compose up --build"     $C up -d --build --wait
t "compose down -v"        $C down -v --remove-orphans
if command -v docker-compose >/dev/null; then t "docker-compose standalone" docker-compose version; fi

# Porto wrappers
t "porto docker ps"        porto docker ps
t "porto ps"               porto ps
t "porto images"           porto images
t "porto docker status"    porto docker status
t "porto compose version"  porto compose version
t "porto docker run --rm"  porto docker run --rm --label $L $IMG echo via-porto

echo
echo "PASS=$PASS FAIL=$FAIL"
for f in "${FAILED[@]+"${FAILED[@]}"}"; do echo "  failed: $f"; done
echo "log: $LOG"
[ "$FAIL" -eq 0 ]
