#!/usr/bin/env bash
# Copyright 2026 NVIDIA CORPORATION
# SPDX-License-Identifier: Apache-2.0
#
# Network use case O4, PoC: inject NIC faults through nvml-mock-ctl and record
# what reaches the kernel log, what NVSentinel's syslog and NIC monitors raise,
# and when the node is cordoned and uncordoned. Records, never asserts: the
# results are read from $DIAG afterwards. Needs NODE and DIAG.
set -uo pipefail

NS=nvsentinel
POD=$(kubectl -n mokka get pod -o name --field-selector "spec.nodeName=$NODE" | head -1)
SPOD() { kubectl -n "$NS" get pod -o name --field-selector "spec.nodeName=$NODE" | grep syslog-health-monitor | head -1; }

now() { date -u +%H:%M:%S.%3N; }
log() { echo "$(now) $*" | tee -a "$DIAG/scenarios.txt"; }
epoch() { date +%s.%N; }
since() { awk -v a="$1" -v b="$(epoch)" 'BEGIN { printf "%.2f", b - a }'; }

# ctl runs nvml-mock-ctl on the node; every wait after it is timed from here.
INJ=$(date +%s.%N)
ctl() {
  INJ=$(epoch)
  log "ctl $*"
  kubectl -n mokka exec "$POD" -c node-agent -- nvml-mock-ctl "$@" 2>&1 | tee -a "$DIAG/scenarios.txt"
}

# wait_until DESC TIMEOUT CMD...: polls CMD every 0.25 s and logs when it held,
# in seconds since the last ctl.
wait_until() {
  local desc=$1 timeout=$2 t0
  shift 2
  t0=$(epoch)
  until "$@" >/dev/null 2>&1; do
    if awk -v a="$t0" -v b="$(epoch)" -v t="$timeout" 'BEGIN { exit !(b - a > t) }'; then
      log "TIMEOUT after ${timeout}s: $desc"
      return 1
    fi
    sleep 0.25
  done
  log "t+$(since "$INJ")s: $desc"
}

cordoned() { [ "$(kubectl get node "$NODE" -o jsonpath='{.spec.unschedulable}')" = true ]; }
uncordoned() { ! cordoned; }
condition() { kubectl get node "$NODE" -o json | jq -e --arg t "$1" --arg s "$2" '.status.conditions[] | select(.type == $t and .status == $s)'; }

# nic_errors: the syslog monitor's count of NIC driver events, by pattern.
nic_errors() {
  local pod
  pod=$(SPOD | cut -d/ -f2)
  kubectl get --raw "/api/v1/namespaces/$NS/pods/$pod:2112/proxy/metrics" 2>/dev/null |
    awk '/^syslog_health_monitor_nic_driver_errors\{/ { n += $2 } END { print n + 0 }'
}
snapshot() {
  local pod
  pod=$(SPOD | cut -d/ -f2)
  {
    echo "== $(now) $1"
    kubectl get --raw "/api/v1/namespaces/$NS/pods/$pod:2112/proxy/metrics" 2>/dev/null |
      grep '^syslog_health_monitor_nic_driver_errors{' | grep -v ' 0$'
    kubectl get node "$NODE" -o json |
      jq -r '"unschedulable=\(.spec.unschedulable // false)", (.status.conditions[] | select(.type | test("Check|SysLogs")) | "\(.type) \(.status) \(.message | .[0:160])")'
  } >>"$DIAG/snapshots.txt"
}
# quiet SECONDS: the syslog monitor raises no NIC event for SECONDS, which
# spans at least two of its 15 s polls.
quiet() {
  local before after
  before=$(nic_errors)
  sleep "$1"
  after=$(nic_errors)
  if [ "$before" = "$after" ]; then
    log "syslog monitor quiet for $1 s (nic_driver_errors stays $after)"
  else
    log "syslog monitor raised $((after - before)) NIC events in $1 s"
  fi
}
# raised BASE N TIMEOUT: wait for the syslog monitor's NIC event count to grow
# by N from BASE.
raised() {
  local base=$1 want=$2 timeout=$3 t0 n
  t0=$(epoch)
  while :; do
    n=$(nic_errors)
    if [ $((n - base)) -ge "$want" ]; then
      log "t+$(since "$INJ")s: syslog monitor counted $((n - base)) new NIC event(s)"
      return 0
    fi
    if awk -v a="$t0" -v b="$(epoch)" -v t="$timeout" 'BEGIN { exit !(b - a > t) }'; then
      log "TIMEOUT after ${timeout}s: syslog monitor counted $((n - base)) of $want NIC event(s)"
      return 1
    fi
    sleep 0.5
  done
}
metrics_served() {
  local pod
  pod=$(SPOD | cut -d/ -f2)
  [ -n "$pod" ] && kubectl get --raw "/api/v1/namespaces/$NS/pods/$pod:2112/proxy/metrics" | grep -q syslog_health_monitor
}

# --- recorder: node state every 0.5 s ----------------------------------------
(
  while true; do
    kubectl get node "$NODE" -o json 2>/dev/null |
      jq -r --arg t "$(now)" '"\($t) unsched=\(.spec.unschedulable // false) " + ([.status.conditions[] | select(.status == "True" and (.type | test("Check|SysLogs"))) | .type] | join(","))'
    sleep 0.5
  done
) >"$DIAG/poll.txt" &
RECORDER=$!
trap 'kill $RECORDER 2>/dev/null' EXIT

# --- S0: baseline ----------------------------------------------------------------
log "S0 baseline"
wait_until "InfiniBandStateCheck reported healthy" 300 condition InfiniBandStateCheck False
wait_until "node schedulable" 60 uncordoned
wait_until "syslog monitor metrics served" 300 metrics_served
# Let the syslog monitor seek to the journal tail before the first fault.
sleep 20
snapshot "S0 baseline"

# --- S1: cable pulled on an InfiniBand NIC ----------------------------------------
log "S1 cable pulled: mlx5_0 (InfiniBand)"
base=$(nic_errors)
ctl nic module --nic mlx5_0 unplugged
wait_until "S1 node cordoned" 60 cordoned
raised "$base" 1 60
snapshot "S1 fault"
ctl nic module --nic mlx5_0 plugged
wait_until "S1 node uncordoned" 60 uncordoned
quiet 35
snapshot "S1 cleared"

# --- S2: RoCE port down ------------------------------------------------------------
log "S2 port down: mlx5_3 (RoCE)"
ctl nic state --nic mlx5_3 --state DOWN --phys-state Disabled
wait_until "S2 node cordoned" 60 cordoned
quiet 35
snapshot "S2 fault"
ctl nic state --nic mlx5_3 --clear
wait_until "S2 node uncordoned" 60 uncordoned
quiet 35

# --- S3: InfiniBand port down, no module change -----------------------------------
log "S3 port down: mlx5_2 (InfiniBand)"
ctl nic state --nic mlx5_2 --state DOWN --phys-state Disabled
wait_until "S3 node cordoned" 60 cordoned
quiet 35
ctl nic state --nic mlx5_2 --clear
wait_until "S3 node uncordoned" 60 uncordoned

# --- S4: NIC falls off the bus -------------------------------------------------------
log "S4 off the bus: mlx5_1"
ctl nic remove --nic mlx5_1
wait_until "S4 node cordoned" 60 cordoned
quiet 35
snapshot "S4 fault"
ctl nic restore --nic mlx5_1
wait_until "S4 node uncordoned" 60 uncordoned
quiet 35
snapshot "S4 cleared"

# --- S5: firmware unrecoverable error, then recovery --------------------------------
log "S5 firmware hw-fatal: mlx5_0"
base=$(nic_errors)
ctl nic health --nic mlx5_0 hw-fatal
wait_until "S5 SysLogsNICDriverError condition True" 60 condition SysLogsNICDriverError True
wait_until "S5 node cordoned" 60 cordoned
raised "$base" 2 30
snapshot "S5 fault"
ctl nic health --nic mlx5_0 ok
quiet 35
wait_until "S5 node uncordoned after recovery (expected never)" 60 uncordoned
snapshot "S5 recovered (driver)"

# --- S6: the other fatal and non-fatal patterns --------------------------------------
log "S6 heartbeat-lost: mlx5_2; cmd-timeout: mlx5_3; module high-temperature: mlx5_1"
base=$(nic_errors)
ctl nic health --nic mlx5_2 heartbeat-lost
ctl nic health --nic mlx5_3 cmd-timeout
ctl nic module --nic mlx5_1 high-temperature
raised "$base" 3 60
snapshot "S6 faults"
ctl nic health --nic mlx5_2 ok
ctl nic health --nic mlx5_3 ok
ctl nic module --nic mlx5_1 plugged
quiet 35
snapshot "S6 cleared"

# --- S7: harness, not Mokka: make the syslog monitor believe the node rebooted -------
# NVSentinel clears its syslog conditions only when the boot ID changes. Rewrite
# the boot ID in its state file and restart it.
log "S7 simulated reboot (syslog monitor state file)"
docker exec "$NODE" sh -c 'find /run /var/lib -name state.json 2>/dev/null' | tee -a "$DIAG/scenarios.txt"
state=$(docker exec "$NODE" sh -c 'find /run /var/lib -name state.json 2>/dev/null | grep syslog | head -1')
docker exec "$NODE" cat "$state" >"$DIAG/s7-state-before.json"
# The pod's log goes with it: keep it.
kubectl -n "$NS" logs "$(SPOD)" --timestamps >"$DIAG/syslog-monitor-before-s7.log" 2>&1
docker exec "$NODE" sed -i 's/"boot_id":"[^"]*"/"boot_id":"00000000-0000-0000-0000-000000000000"/' "$state"
INJ=$(epoch)
kubectl -n "$NS" delete "$(SPOD)" --grace-period=0 --force 2>&1 | tee -a "$DIAG/scenarios.txt"
wait_until "S7 SysLogsNICDriverError condition False" 120 condition SysLogsNICDriverError False
wait_until "S7 node uncordoned" 60 uncordoned
sleep 40
snapshot "S7 after simulated reboot"
docker exec "$NODE" cat "$state" >"$DIAG/s7-state-after.json"

log "done"
