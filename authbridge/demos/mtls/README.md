# mTLS demo — agent-to-agent encryption via SPIRE X.509 SVIDs

Two pods that talk to each other through their authbridge sidecars,
configured with `mtls.mode: strict`. Confirms end-to-end:

- authbridge's `spiffe.Provider` mirror writes `/opt/svid.pem` /
  `_key.pem` / `_bundle.pem` on both pods on every rotation; the files
  exist for external readers (e2e probes, debugging, future Envoy
  filesystem SDS).
- For the listener itself, authbridge reads X.509 SVIDs in-memory via
  `spiffe.X509Source` (no per-handshake file I/O) and uses them for
  both inbound (reverse proxy) and outbound (forward proxy) mTLS.
- The wire between the two pods is encrypted: a `tcpdump` on the
  cluster network shows no plaintext HTTP request lines on TCP port
  8080.
- abctl shows the negotiated TLS version + peer SPIFFE ID per session
  event in the detail pane.

This is intentionally minimal. It doesn't exercise IBAC, weather, or
the token-exchange flows — it just proves the mTLS layer is real.

## Prerequisites

1. **A kagenti install on a kind cluster** with SPIRE enabled.
2. **kubectl, kind, podman (or docker)** on PATH.

## Quick start

```sh
cd authbridge/demos/mtls

make demo-mtls          # deploys caller + callee, both with mtls.mode: strict
                        # waits for ready, runs an end-to-end request,
                        # asserts the wire was encrypted via tcpdump

make demo-mtls-permissive
                        # same shape but with mtls.mode: permissive
                        # sends a plaintext curl from outside the mesh,
                        # asserts the request is served (proving permissive
                        # serves both)

make demo-mtls-strict-rejects-plain
                        # hits a strict callee with plain HTTP and asserts
                        # the connection is closed (proving strict actually
                        # enforces)

make undeploy           # cleanup
```

## Verification recipe

The `make demo-mtls` target does:

1. Deploys caller + callee Pods to `team1` with operator-injected
   sidecars and SPIRE.
2. Patches the operator-rendered `authbridge-config-{caller,callee}`
   ConfigMaps to add `mtls: { mode: strict }`.
3. Restarts the pods so the new config takes effect (mTLS config
   requires pod restart per the framework hot-reload boundary).
4. Triggers a request from caller → callee through the kagenti UI
   flow (caller's outbound forward proxy → callee's inbound reverse
   proxy).
5. Captures cluster traffic via `tcpdump -i any -n -A 'tcp port 8080'`
   inside the kind cluster's network namespace. Asserts no plaintext
   HTTP/1.1 request lines appear in captured packets.
6. Greps both pods' logs for the startup line confirming mTLS is
   enabled (`"mTLS enabled" mode=strict`).
7. Reports the negotiated TLS version + peer SPIFFE ID from the
   session API.

## What this demo doesn't cover

- **Permissive vs strict rollout**: covered by `make demo-mtls-permissive`
  and `make demo-mtls-strict-rejects-plain`.
- **Cert rotation under load**: tested via unit tests in
  `authlib/spiffe/x509source_test.go`.
- **kagenti UI integration**: this demo is intentionally pre-UI —
  if you want to chat with an mTLS-protected agent, run any other
  kagenti demo with `mtls: { mode: strict }` added to its config.
- **Ambient mesh interaction**: when ambient is on, ztunnel handles
  the cross-pod hop and authbridge's mTLS is redundant. This demo
  doesn't run ambient. Phase 5 will add ambient detection so the
  redundancy is auto-dropped.
