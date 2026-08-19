#!/usr/bin/env bash

# PROTOTYPE: throw this branch away after the managed-runtime decision is
# absorbed. This models only the workload/platform seam; it is not an operator,
# Helm chart, or production manifest.

set -euo pipefail

image="${TYPECLAW_PROTOTYPE_IMAGE:-typeclaw-runtime:managed-final}"
cluster_name="${TYPECLAW_PROTOTYPE_CLUSTER:-typeclaw-mr-${RANDOM}}"
namespace="typeclaw-prototype"
keep_cluster="${TYPECLAW_PROTOTYPE_KEEP_CLUSTER:-0}"
cluster_created=0

for command_name in docker kind kubectl jq; do
  command -v "$command_name" >/dev/null || {
    echo "missing required command: $command_name" >&2
    exit 1
  }
done

[[ "$image" =~ ^[A-Za-z0-9][A-Za-z0-9./:_-]*$ ]] || {
  echo "unsafe image reference: $image" >&2
  exit 1
}
[[ "$cluster_name" =~ ^[a-z0-9][a-z0-9-]*$ ]] || {
  echo "unsafe kind cluster name: $cluster_name" >&2
  exit 1
}

docker image inspect "$image" >/dev/null
if ! existing_clusters="$(kind get clusters)"; then
  echo "failed to establish current kind cluster names" >&2
  exit 1
fi
if grep -Fxq "$cluster_name" <<<"$existing_clusters"; then
  echo "refusing to reuse existing kind cluster: $cluster_name" >&2
  exit 1
fi

cleanup() {
  if [[ "$cluster_created" != "1" ]]; then
    return
  fi
  if [[ "$keep_cluster" == "1" ]]; then
    echo "prototype cluster retained: $cluster_name" >&2
  else
    if ! kind delete cluster --name "$cluster_name" >/dev/null; then
      echo "failed to delete prototype cluster: $cluster_name" >&2
    fi
  fi
}
trap cleanup EXIT

echo "Question: can the proposed upstream Managed Runtime satisfy the Kubernetes workload contract?"
echo "Image: $image"
echo "Creating ephemeral kind cluster: $cluster_name"
kind create cluster --name "$cluster_name" --wait 180s
cluster_created=1
kind load docker-image "$image" --name "$cluster_name"

kubectl apply -f - <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: ${namespace}
---
apiVersion: v1
kind: PersistentVolume
metadata:
  name: typeclaw-prototype-agent
spec:
  capacity:
    storage: 1Gi
  accessModes: [ReadWriteOnce]
  persistentVolumeReclaimPolicy: Retain
  storageClassName: ""
  hostPath:
    path: /var/local/typeclaw-prototype/agent
    type: DirectoryOrCreate
---
apiVersion: v1
kind: PersistentVolume
metadata:
  name: typeclaw-prototype-home
spec:
  capacity:
    storage: 1Gi
  accessModes: [ReadWriteOnce]
  persistentVolumeReclaimPolicy: Retain
  storageClassName: ""
  hostPath:
    path: /var/local/typeclaw-prototype/home
    type: DirectoryOrCreate
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: agent
  namespace: ${namespace}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: ""
  volumeName: typeclaw-prototype-agent
  resources:
    requests:
      storage: 1Gi
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: runtime-home
  namespace: ${namespace}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: ""
  volumeName: typeclaw-prototype-home
  resources:
    requests:
      storage: 1Gi
---
apiVersion: v1
kind: Service
metadata:
  name: typeclaw
  namespace: ${namespace}
spec:
  clusterIP: None
  selector:
    app: typeclaw-prototype
  ports:
    - name: runtime
      port: 8973
      targetPort: runtime
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: typeclaw
  namespace: ${namespace}
spec:
  serviceName: typeclaw
  replicas: 1
  selector:
    matchLabels:
      app: typeclaw-prototype
  template:
    metadata:
      labels:
        app: typeclaw-prototype
    spec:
      automountServiceAccountToken: false
      terminationGracePeriodSeconds: 60
      securityContext:
        fsGroup: 65532
      initContainers:
        - name: prepare-runtime-volumes
          image: ${image}
          imagePullPolicy: Never
          command: [sh, -ec]
          args:
            - |
              umask 077
              mkdir -p /init/agent/node_modules/typeclaw-gws-multi-account /init/agent/workspace /init/home /init/control /init/tmp /init/shm
              if [ ! -e /init/agent/typeclaw.json ]; then
                printf '%s\n' '{}' > /init/agent/typeclaw.json
              fi
              if [ ! -e /init/agent/secrets.json ]; then
                printf '%s\n' '{"version":2,"providers":{"openai":{"type":"api_key","key":"prototype-sentinel"}},"channels":{}}' > /init/agent/secrets.json
              fi
              if [ ! -e /init/agent/node_modules/typeclaw-gws-multi-account/package.json ]; then
                printf '%s\n' '{"name":"typeclaw-gws-multi-account","version":"0.0.0-stale","type":"module","main":"index.js"}' > /init/agent/node_modules/typeclaw-gws-multi-account/package.json
                printf '%s\n' 'throw new Error("stale Agent Folder GWS package was loaded")' > /init/agent/node_modules/typeclaw-gws-multi-account/index.js
              fi
              if [ ! -e /init/agent/workspace/prototype-tool.sh ]; then
                printf '%s\n' '#!/bin/sh' 'printf "%s\\n" executable-mode-preserved' > /init/agent/workspace/prototype-tool.sh
                chmod 0755 /init/agent/workspace/prototype-tool.sh
              fi
              chown -R 65532:65532 /init/agent /init/home /init/control /init/tmp /init/shm
              chmod 0700 /init/agent /init/home /init/control
              chmod 0600 /init/agent/secrets.json
              chmod 1777 /init/tmp /init/shm
          securityContext:
            runAsUser: 0
            runAsNonRoot: false
            allowPrivilegeEscalation: false
            capabilities:
              drop: [ALL]
              add: [CHOWN, DAC_OVERRIDE, FOWNER]
            seccompProfile:
              type: RuntimeDefault
          volumeMounts:
            - { name: agent, mountPath: /init/agent }
            - { name: runtime-home, mountPath: /init/home }
            - { name: managed-control, mountPath: /init/control }
            - { name: runtime-tmp, mountPath: /init/tmp }
            - { name: browser-shm, mountPath: /init/shm }
      containers:
        - name: runtime
          image: ${image}
          imagePullPolicy: Never
          args: [run, --no-tui]
          env:
            - { name: TYPECLAW_RUNTIME_ID, value: typeclaw-prototype-0 }
            - { name: TYPECLAW_MANAGED_CONTROL_DIR, value: /run/typeclaw-managed }
          ports:
            - { name: runtime, containerPort: 8973 }
          startupProbe:
            httpGet: { path: /health/live, port: runtime }
            periodSeconds: 2
            failureThreshold: 90
          readinessProbe:
            httpGet: { path: /health/ready, port: runtime }
            periodSeconds: 2
            failureThreshold: 3
          livenessProbe:
            httpGet: { path: /health/live, port: runtime }
            periodSeconds: 10
            failureThreshold: 3
          securityContext:
            runAsNonRoot: true
            runAsUser: 65532
            runAsGroup: 65532
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: [ALL]
            seccompProfile:
              type: Unconfined
          volumeMounts:
            - { name: agent, mountPath: /agent }
            - { name: runtime-home, mountPath: /home/typeclaw }
            - { name: managed-control, mountPath: /run/typeclaw-managed }
            - { name: runtime-tmp, mountPath: /tmp }
            - { name: browser-shm, mountPath: /dev/shm }
        - name: relay
          image: ${image}
          imagePullPolicy: Never
          command: [bun, -e]
          args:
            - |
              import { copyFile, readdir, rename, writeFile } from "node:fs/promises"

              let terminating = false
              let terminationDeadline = 0
              let restartObserved = false
              let stoppingObserved = false
              process.on("SIGTERM", () => {
                terminating = true
                terminationDeadline = Date.now() + 10_000
              })
              const sleep = (milliseconds) => new Promise((resolve) => setTimeout(resolve, milliseconds))

              while (true) {
                if (!restartObserved) {
                  const names = await readdir("/run/typeclaw-managed")
                  for (const name of names) {
                    if (!name.startsWith("restart-") || !name.endsWith(".json")) continue
                    await copyFile("/run/typeclaw-managed/" + name, "/run/typeclaw-managed/relay-observed.json.tmp")
                    await rename("/run/typeclaw-managed/relay-observed.json.tmp", "/run/typeclaw-managed/relay-observed.json")
                    restartObserved = true
                  }
                }

                if (restartObserved && !stoppingObserved) {
                  try {
                    const [live, ready] = await Promise.all([
                      fetch("http://127.0.0.1:8973/health/live"),
                      fetch("http://127.0.0.1:8973/health/ready"),
                    ])
                    const liveBody = await live.json()
                    const readyBody = await ready.json()
                    if (live.status === 200 && ready.status === 503 && readyBody.status === "stopping" && readyBody.ready === false) {
                      await writeFile(
                        "/home/typeclaw/prototype-stopping-observed.json",
                        JSON.stringify({ liveStatus: live.status, readyStatus: ready.status, liveBody, readyBody }) + "\n",
                        { mode: 0o600 },
                      )
                      stoppingObserved = true
                    }
                  } catch {
                    // The runtime may close between polls. If no transition was
                    // captured, the termination deadline makes the run red.
                  }
                }
                if (terminating) {
                  if (stoppingObserved) process.exit(0)
                  if (Date.now() >= terminationDeadline) {
                    await writeFile(
                      "/home/typeclaw/prototype-stopping-observed.json",
                      JSON.stringify({ error: "stopping health transition was not observed" }) + "\n",
                      { mode: 0o600 },
                    )
                    process.exit(1)
                  }
                }
                await sleep(restartObserved ? 1 : 10)
              }
          securityContext:
            runAsNonRoot: true
            runAsUser: 65532
            runAsGroup: 65532
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: [ALL]
            seccompProfile:
              type: RuntimeDefault
          volumeMounts:
            - { name: managed-control, mountPath: /run/typeclaw-managed }
            - { name: runtime-home, mountPath: /home/typeclaw }
      volumes:
        - name: agent
          persistentVolumeClaim: { claimName: agent }
        - name: runtime-home
          persistentVolumeClaim: { claimName: runtime-home }
        - name: managed-control
          emptyDir: {}
        - name: runtime-tmp
          emptyDir: { medium: Memory, sizeLimit: 256Mi }
        - name: browser-shm
          emptyDir: { medium: Memory, sizeLimit: 512Mi }
EOF

kubectl -n "$namespace" rollout status statefulset/typeclaw --timeout=300s
pod="typeclaw-0"

runtime_uid="$(kubectl -n "$namespace" exec "$pod" -c runtime -- id -u)"
runtime_gid="$(kubectl -n "$namespace" exec "$pod" -c runtime -- id -g)"
[[ "$runtime_uid:$runtime_gid" == "65532:65532" ]]
kubectl -n "$namespace" exec "$pod" -c runtime -- sh -ec '
  awk '\''$5 == "/" && $6 ~ /(^|,)ro(,|$)/ { found = 1 } END { exit found ? 0 : 1 }'\'' /proc/self/mountinfo
  test ! -e /var/run/secrets/kubernetes.io/serviceaccount/token
  touch /agent/.prototype-write
  rm /agent/.prototype-write
  test "$(stat -c %a /agent/workspace/prototype-tool.sh)" = 755
  test "$(/agent/workspace/prototype-tool.sh)" = executable-mode-preserved
'
kubectl -n "$namespace" exec "$pod" -c relay -- sh -ec '
  test ! -e /var/run/secrets/kubernetes.io/serviceaccount/token
'

kubectl -n "$namespace" exec "$pod" -c runtime -- bun -e '
  for (const path of ["/health/live", "/health/ready"]) {
    const response = await fetch("http://127.0.0.1:8973" + path)
    const body = await response.json()
    if (response.status !== 200 || body.schemaVersion !== 1 || body.status !== "ready" || body.ready !== true || body.degraded !== false) {
      throw new Error(path + ": status=" + response.status + " body=" + JSON.stringify(body))
    }
  }
'

kubectl -n "$namespace" exec "$pod" -c runtime -- sh -ec '
  test ! -e /node_modules/node_modules
  cd /node_modules/typeclaw
  bun -e '\''await import("zod")'\''
'
kubectl -n "$namespace" exec "$pod" -c runtime -- bun -e '
  import { GWS_MULTI_ACCOUNT_PLUGIN_PACKAGE } from "/node_modules/typeclaw/src/config/index.ts"
  import { createManagedDefaultPluginLoader } from "/node_modules/typeclaw/src/run/index.ts"
  const load = createManagedDefaultPluginLoader("managed", [])
  if (!load) throw new Error("managed default plugin loader was not composed")
  const resolved = await load(GWS_MULTI_ACCOUNT_PLUGIN_PACKAGE, "/agent")
  const baked = await Bun.file("/node_modules/" + GWS_MULTI_ACCOUNT_PLUGIN_PACKAGE + "/package.json").json()
  if (resolved.version !== baked.version || resolved.version === "0.0.0-stale") {
    throw new Error("managed GWS resolved " + resolved.version + ", image owns " + baked.version)
  }
'

kubectl -n "$namespace" exec "$pod" -c runtime -- bwrap \
  --unshare-all \
  --new-session \
  --die-with-parent \
  --clearenv \
  --setenv PATH /usr/local/bin:/usr/bin:/bin \
  --setenv HOME /tmp \
  --setenv LANG C.UTF-8 \
  --ro-bind /usr /usr \
  --ro-bind /etc /etc \
  --dev /dev \
  --tmpfs /tmp \
  --ro-bind-try /bin /bin \
  --ro-bind-try /sbin /sbin \
  --ro-bind-try /lib /lib \
  --ro-bind-try /lib64 /lib64 \
  --ro-bind /proc /proc \
  --chdir /tmp \
  bash -c 'test -r /proc/self/maps && test ! -w /usr'
kubectl -n "$namespace" exec "$pod" -c runtime -- agent-browser --version

old_pod_uid="$(kubectl -n "$namespace" get pod "$pod" -o jsonpath='{.metadata.uid}')"
echo "Checking writable secret and runtime-home persistence"
kubectl -n "$namespace" exec "$pod" -c runtime -- bun -e '
  import { writeFile } from "node:fs/promises"
  import { createRuntimeCapabilities } from "/node_modules/typeclaw/src/capabilities/index.ts"
  const capabilities = createRuntimeCapabilities(process.env, "/agent/secrets.json")
  if (capabilities.secrets === null || capabilities.restarter === null) {
    throw new Error("managed runtime capabilities were not resolved")
  }
  await capabilities.secrets.writeBackChannelBlock({ discord: { currentAccount: null, accounts: {} } })
  await writeFile("/home/typeclaw/prototype-home-marker", "survives-replacement\n", { mode: 0o600 })
'
echo "Requesting restart through the live TypeClaw WebSocket server"
kubectl -n "$namespace" exec "$pod" -c runtime -- bun -e '
  await new Promise((resolve, reject) => {
    const socket = new WebSocket("ws://127.0.0.1:8973")
    const timeout = setTimeout(() => reject(new Error("timeout waiting for live restart acceptance")), 15_000)
    socket.addEventListener("error", (event) => reject(event))
    socket.addEventListener("message", (event) => {
      const message = JSON.parse(String(event.data))
      if (message.type === "connected") {
        socket.send(JSON.stringify({ type: "restart" }))
      }
      if (message.type === "restart_result") {
        clearTimeout(timeout)
        socket.close()
        if (message.status !== "accepted") reject(new Error("live restart rejected: " + JSON.stringify(message)))
        else resolve()
      }
    })
  })
'
echo "Live TypeClaw server accepted the restart request"

for _ in $(seq 1 100); do
  if kubectl -n "$namespace" exec "$pod" -c relay -- test -f /run/typeclaw-managed/relay-observed.json >/dev/null 2>&1; then
    break
  fi
  sleep 0.2
done
kubectl -n "$namespace" exec "$pod" -c relay -- test -f /run/typeclaw-managed/relay-observed.json
kubectl -n "$namespace" exec "$pod" -c relay -- bun -e '
  const request = await Bun.file("/run/typeclaw-managed/relay-observed.json").json()
  if (request.schemaVersion !== 1 || request.kind !== "restart" || request.runtimeId !== "typeclaw-prototype-0" || !request.requestId) {
    throw new Error("unexpected restart request: " + JSON.stringify(request))
  }
'
[[ "$(kubectl -n "$namespace" get pod "$pod" -o jsonpath='{.metadata.uid}')" == "$old_pod_uid" ]]
[[ "$(kubectl -n "$namespace" get pod "$pod" -o jsonpath='{.status.containerStatuses[?(@.name=="runtime")].ready}')" == "true" ]]

deletion_started_at="$(date +%s)"
kubectl -n "$namespace" delete pod "$pod" --wait=true --timeout=120s
deletion_finished_at="$(date +%s)"
graceful_shutdown_seconds="$((deletion_finished_at - deletion_started_at))"
if (( graceful_shutdown_seconds >= 20 )); then
  echo "runtime did not exit within the bounded graceful-shutdown window: ${graceful_shutdown_seconds}s" >&2
  exit 1
fi
echo "Runtime Pod exited gracefully in ${graceful_shutdown_seconds}s"
kubectl -n "$namespace" rollout status statefulset/typeclaw --timeout=300s
kubectl -n "$namespace" wait --for=condition=Ready "pod/$pod" --timeout=300s
new_pod_uid="$(kubectl -n "$namespace" get pod "$pod" -o jsonpath='{.metadata.uid}')"
[[ "$new_pod_uid" != "$old_pod_uid" ]]

kubectl -n "$namespace" exec "$pod" -c runtime -- bun -e '
  const secrets = await Bun.file("/agent/secrets.json").json()
  const openaiKey = secrets.providers?.openai?.key
  const openaiKeyValue = typeof openaiKey === "string" ? openaiKey : openaiKey?.value
  if (
    secrets.version !== 2 ||
    secrets.channels?.discord?.currentAccount !== null ||
    secrets.providers?.openai?.type !== "api_key" ||
    openaiKeyValue !== "prototype-sentinel"
  ) {
    throw new Error("writable secrets did not survive replacement: " + JSON.stringify(secrets))
  }
  const marker = await Bun.file("/home/typeclaw/prototype-home-marker").text()
  if (marker.trim() !== "survives-replacement") throw new Error("runtime HOME did not survive replacement")
  const stopping = await Bun.file("/home/typeclaw/prototype-stopping-observed.json").json()
  if (stopping.liveStatus !== 200 || stopping.readyStatus !== 503 || stopping.readyBody?.status !== "stopping") {
    throw new Error("stopping health transition was not observed: " + JSON.stringify(stopping))
  }
  const handoff = await Bun.file("/agent/.typeclaw/restart-pending.json").json()
  if (
    handoff.schemaVersion !== 2 ||
    handoff.origin?.kind !== "tui" ||
    typeof handoff.originatingSessionId !== "string" ||
    typeof handoff.originatingSessionFile !== "string"
  ) {
    throw new Error("restart handoff did not survive replacement: " + JSON.stringify(handoff))
  }
  const ready = await fetch("http://127.0.0.1:8973/health/ready")
  const body = await ready.json()
  if (ready.status !== 200 || body.status !== "ready" || body.degraded !== false) {
    throw new Error("replacement runtime is not cleanly ready: " + JSON.stringify(body))
  }
'
kubectl -n "$namespace" exec "$pod" -c runtime -- sh -ec '
  test "$(stat -c %a /agent/workspace/prototype-tool.sh)" = 755
  test "$(/agent/workspace/prototype-tool.sh)" = executable-mode-preserved
'

server_version="$(kubectl version -o json | jq -r '.serverVersion.gitVersion')"
image_id="$(docker image inspect "$image" --format '{{.Id}}')"
jq -n \
  --arg question "Can the proposed upstream Managed Runtime satisfy the Kubernetes workload contract?" \
  --arg verdict "yes-for-the-proposed-contract; no-for-current-official-release" \
  --arg image "$image" \
  --arg image_id "$image_id" \
  --arg kubernetes "$server_version" \
  --arg old_pod_uid "$old_pod_uid" \
  --arg new_pod_uid "$new_pod_uid" \
  --argjson graceful_shutdown_seconds "$graceful_shutdown_seconds" \
  '{question:$question,verdict:$verdict,image:$image,image_id:$image_id,kubernetes:$kubernetes,observed:{nonRoot:true,readOnlyRoot:true,noServiceAccountToken:true,pvcContinuity:true,pvcModesPreserved:true,healthReady:true,healthStopping:true,writableSecrets:true,restartViaLiveServer:true,restartSpool:true,restartHandoff:true,externalReplacement:true,boundedGracefulShutdown:true,bubblewrap:true,immutableDependencies:true},oldPodUid:$old_pod_uid,newPodUid:$new_pod_uid,gracefulShutdownSeconds:$graceful_shutdown_seconds}'
