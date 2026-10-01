# fluxcd-ui

A small, fast web UI for [Flux](https://fluxcd.io). It lists the Flux objects of a cluster on a single screen with their reconciliation state, and updates live as they change.

Supported kinds:
- GitRepositories, OCIRepositories, Buckets, HelmRepositories, HelmCharts (`source.toolkit.fluxcd.io/v1`)
- HelmReleases (`helm.toolkit.fluxcd.io/v2`)
- Kustomizations (`kustomize.toolkit.fluxcd.io/v1`)

Clicking an object opens a detail drawer: its key fields, conditions, the recent Kubernetes Events of the object and of the sources it reconciles from, the objects a Kustomization or HelmRelease manages and their health (from its inventory, with links to the Flux objects among them), a HelmRelease's release history, and, on demand, what its Flux controller recently logged about it.

It requires **Flux 2.6+**, the first release where all of these are GA. If the cluster doesn't serve a kind, its section shows "not installed". Kinds are detected at startup, so restart the UI after installing new Flux CRDs.

By default the UI is read-only. With `--enable-actions`, the detail drawer can also suspend, resume and reconcile objects (see [Actions and authentication](#actions-and-authentication)).

## How it works

- **Backend:** Go, using controller-runtime informers. All objects are held in an in-memory cache, so page loads never hit the API server.
- **Live updates:** watch events are debounced and pushed to browsers over server-sent events (`/events`). htmx then re-fetches only the section that changed. If the connection drops, the page reconnects and refreshes.
- **Frontend:** server-rendered `html/template`, Tailwind CSS v4 and htmx. htmx is vendored and no external fonts are used, so it works on clusters without internet access.
- **Theme:** follows the OS light/dark setting until you click the toggle in the header. The choice is kept in a `theme` cookie, so the server renders it directly and the page never flashes the wrong theme.
- **Filters:** search, namespace and status filters live in the URL, so a filtered view can be shared.

## Running locally

The UI uses your current kubeconfig context and only needs `get`/`list`/`watch` on the kinds above.

```sh
make run                      # http://localhost:8080
make run ARGS="--addr :9090 --kubeconfig ~/.kube/other"
```

Flags:

| Flag | Default | |
|---|---|---|
| `--addr` | `:8080` | HTTP listen address |
| `--kubeconfig` | | Path to a kubeconfig. If unset, uses in-cluster config or `$KUBECONFIG`/`~/.kube/config` |
| `--log-level` | `info` | `debug`, `info`, `warn` or `error` |
| `--log-format` | `text` | `text` or `json` |
| `--enable-actions` | `false` | Allow suspend, resume and reconcile. Requires `patch` RBAC |
| `--user-header` | | Request header carrying the user name set by an authenticating proxy. When set, actions require it |
| `--groups-header` | | Request header carrying the user's groups set by the proxy |
| `--groups-separator` | `,` | Separator of the groups header (`\|` for authentik) |
| `--authorization` | `none` | Who may act: `none` (every user) or `rbac` (Kubernetes RBAC, see below) |
| `--subject-prefix` | `fluxcd-ui:` | Prefix added to user and group names before checking RBAC |
| `--flux-namespace` | `flux-system` | Namespace of the Flux controllers |
| `--managed-objects-status` | `true` | Show the health of the objects a Kustomization or HelmRelease manages. Requires read access to them |
| `--controller-logs` | `true` | Offer the controllers' logs about an object in its drawer. Requires `list pods` and `get pods/log` in `--flux-namespace` |
| `--version` | | Print the version and exit |

Endpoints: `/healthz` returns ok once the process is up. `/readyz` returns ok once every kind has either synced or reported a watch error.

## Deploying with Helm

```sh
helm install fluxcd-ui charts/fluxcd-ui -n fluxcd-ui --create-namespace
kubectl -n fluxcd-ui port-forward svc/fluxcd-ui 8080:80
```

The chart creates:
- a ServiceAccount,
- a read-only ClusterRole on Flux objects and events, and its binding (`rbac.create`); `actions.enabled` adds `patch`,
- a Role reading the controllers' pods and logs in `fluxNamespace` (`logs.enabled`),
- a binding to the built-in `view` ClusterRole, to check the health of managed objects (`managedObjects.status.enabled`). `view` reads most resources but never Secrets; kinds it does not cover show as "no access" unless you grant them with `managedObjects.status.extraRules`,
- a Deployment that runs as non-root with a read-only root filesystem and all capabilities dropped,
- a Service, and an optional Ingress.

See [`values.yaml`](charts/fluxcd-ui/values.yaml) for all options.

> **Security:** there is no built-in authentication. Even read-only, the UI shows URLs, revisions and error messages from across the cluster. Expose it only behind an authenticating proxy, such as an authentik outpost or oauth2-proxy.

Behind ingress-nginx, live updates work out of the box: the SSE response sets `X-Accel-Buffering: no` and sends a heartbeat every 20s.

## Actions and authentication

With actions enabled (`--enable-actions`, or `actions.enabled=true` in the chart), the detail drawer offers:

| Action | What it does | flux CLI equivalent |
|---|---|---|
| Reconcile | Sets `reconcile.fluxcd.io/requestedAt`. A HelmRelease also gets its HelmChart reconciled first. | `flux reconcile <kind> <name>` |
| Reconcile with source | Reconciles the source chain first (e.g. HelmRepository → HelmChart → HelmRelease), then the object. | `flux reconcile <kind> <name> --with-source` |
| Suspend / Resume | Sets `spec.suspend`. Resuming makes the controller reconcile right away. | `flux suspend` / `flux resume` |

A reconcile request shows as "reconcile requested" until the controller picks it up. Reconcile is not offered on suspended objects or on OCI HelmRepositories, because Flux would ignore it.

Every action is logged with the object and the user. Actions are POST requests, and cross-site requests are rejected (CSRF protection).

### Authentication through a proxy

Changes are made with the UI's own service account, so **anyone who can reach the UI can suspend or reconcile anything**. Enable actions only behind an authenticating proxy, and tell the UI which header carries the authenticated user:

| Proxy | `auth.userHeader` | `auth.groupsHeader` (separator) |
|---|---|---|
| authentik (proxy provider, forward auth) | `X-authentik-username` | `X-authentik-groups` (`\|`) |
| oauth2-proxy as a reverse proxy, with `--pass-user-headers` (Okta, Dex, Keycloak, Google, Entra ID…) | `X-Forwarded-User` | `X-Forwarded-Groups` (`,`) |
| oauth2-proxy with `--set-xauthrequest` behind nginx `auth_request` | `X-Auth-Request-User` | `X-Auth-Request-Groups` (`,`) |

Groups are only needed for [RBAC authorization](#authorization-with-kubernetes-rbac). The identity provider must put them in its tokens. With Dex, request the `groups` scope. With Okta, add a `groups` claim to the authorization server. With Keycloak, add a group mapper. authentik includes them by default.

With the header configured, actions without it are refused (401), and the user name is logged with each action and shown in the header bar.

Example with authentik and Traefik: create a forward-auth Middleware pointing at the outpost, and copy the identity headers onto requests. Traefik replaces client-supplied copies of these headers, so they can't be spoofed through the ingress.

```yaml
apiVersion: traefik.io/v1alpha1
kind: Middleware
metadata:
  name: authentik
  namespace: fluxcd-ui
spec:
  forwardAuth:
    address: http://ak-outpost-authentik-embedded-outpost.authentik.svc:9000/outpost.goauthentik.io/auth/traefik
    trustForwardHeader: true
    authResponseHeaders:
      - X-authentik-username
      - X-authentik-groups
      - X-authentik-email
```

```yaml
# values.yaml
actions:
  enabled: true
auth:
  userHeader: X-authentik-username
ingress:
  enabled: true
  annotations:
    traefik.ingress.kubernetes.io/router.middlewares: fluxcd-ui-authentik@kubernetescrd
  hosts:
    - host: flux.example.com
      paths: [{ path: /, pathType: Prefix }]
```

See authentik's Traefik integration guide for the outpost side, including the route for `/outpost.goauthentik.io/`. Restrict who may open the application with an authentik policy binding, e.g. to a `flux-operators` group.

The header can only be trusted if every request goes through the proxy. Pods inside the cluster can still reach the Service directly and set it themselves. To close that path, allow ingress only from the ingress controller:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: fluxcd-ui
  namespace: fluxcd-ui
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: fluxcd-ui
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: kube-system # where Traefik runs on k3s
```

Traffic from the pod's own node, such as kubelet probes, is always allowed.

### Authorization with Kubernetes RBAC

With `authorization.mode=rbac` (`--authorization=rbac`), fluxcd-ui checks Kubernetes RBAC before each action. It asks the API server, with a SubjectAccessReview, whether the proxy's user and groups are granted a verb on the object:

| Action | Verb checked |
|---|---|
| Reconcile | `reconcile` on the object |
| Reconcile with source | `reconcile` on the object and on each source it refreshes (a HelmRelease's own HelmChart is covered by the HelmRelease) |
| Suspend, Resume | `suspend` on the object |

These verbs mean nothing to the API server. They only let fluxcd-ui act on the user's behalf, so granting them gives nobody direct API access. The changes themselves are still made with fluxcd-ui's service account.

The drawer only shows the buttons the user may use. Those answers are cached for 30 seconds, and every action is checked again uncached. The UI stays read-only for everyone else.

User and group names are prefixed with `authorization.subjectPrefix` (default `fluxcd-ui:`) before the check. That way, a header can never name a built-in identity such as `system:masters`. If your API server authenticates kubectl users with the same identity provider, you can set the prefix to the API server's OIDC prefix (e.g. `oidc:`), so the same names appear in both places.

The chart creates two ClusterRoles to bind: `fluxcd-ui-operator` (reconcile, suspend) and `fluxcd-ui-reconciler` (reconcile), named after the release (here `fluxcd-ui`). For example:

```yaml
# Flux admins: everything, everywhere.
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: fluxcd-ui-flux-admins
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: fluxcd-ui-operator}
subjects: [{apiGroup: rbac.authorization.k8s.io, kind: Group, name: "fluxcd-ui:flux-admins"}]
---
# team-a: reconcile only, in its own namespace.
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: fluxcd-ui-team-a
  namespace: apps
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: fluxcd-ui-reconciler}
subjects: [{apiGroup: rbac.authorization.k8s.io, kind: Group, name: "fluxcd-ui:team-a"}]
```

Check a grant from the command line (kubectl warns that the verb is unknown, which is expected for custom verbs):

```sh
kubectl auth can-i reconcile helmreleases.helm.toolkit.fluxcd.io -n apps \
  --as fluxcd-ui:alice --as-group fluxcd-ui:team-a
```

This trusts the user and groups headers completely: a forged groups header is a privilege escalation. Use the NetworkPolicy above, or make sure nothing but the proxy can reach the UI.

## Building

```sh
make build                    # bin/fluxcd-ui; downloads the Tailwind standalone CLI into bin/
make test
make lint                     # golangci-lint, as in CI
make docker-build             # registry.mkz.me/mycroft/fluxcd-ui:dev
make docker-build CONTAINER_TOOL=podman IMAGE=registry.example.com/fluxcd-ui TAG=v0.1.0
```

The Dockerfile needs BuildKit: Docker with buildx, or podman. It cross-compiles, so a multi-arch image is a single build:

```sh
docker buildx build --platform linux/amd64,linux/arm64 -t registry.mkz.me/mycroft/fluxcd-ui:0.1.0 --push .
```

## CI and releases

Workflows live in `.gitea/workflows` and run on Gitea Actions:

| Workflow | Runs on | What it does |
|---|---|---|
| `test.yaml` | push to `main`, pull requests | `go build`, then `go test -race` |
| `lint.yaml` | push to `main`, pull requests | `go mod tidy -diff` and golangci-lint (`.golangci.yaml`). Separately, strict `helm lint` and kubeconform on the chart, rendered with default values and with `charts/fluxcd-ui/ci/full-values.yaml` |
| `build-image.yaml` | push to `main`, `v*` tags, manual | Builds and pushes `registry.mkz.me/mycroft/fluxcd-ui`: `latest` from `main`, `X.Y.Z` from tag `vX.Y.Z`, plus `sha-<commit>` and `build-<run>` on every build |
| `release-chart.yaml` | `v*` tags | Packages the chart as version and appVersion `X.Y.Z` and pushes it to `oci://registry.mkz.me/mycroft/charts` |

The image and chart workflows need two repository secrets: `REGISTRY_USERNAME` and `REGISTRY_PASSWORD`, for an account (e.g. a Harbor robot account) that can push to the `mycroft` project.

To release, tag and push:

```sh
git tag v0.1.0 && git push origin v0.1.0
```

The tag produces image `0.1.0` and chart `0.1.0`, whose default image tag is that image. `Chart.yaml`'s own version only matters for local installs.

Install the published chart with Flux:

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: fluxcd-ui
  namespace: flux-system
spec:
  interval: 1h
  url: oci://registry.mkz.me/mycroft/charts/fluxcd-ui
  ref:
    semver: "0.x"
  layerSelector:
    mediaType: application/vnd.cncf.helm.chart.content.v1.tar+gzip
    operation: copy
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: fluxcd-ui
  namespace: fluxcd-ui
spec:
  interval: 1h
  chartRef:
    kind: OCIRepository
    name: fluxcd-ui
    namespace: flux-system
  values:
    actions:
      enabled: true
```

The `mycroft` project allows anonymous pulls, so neither needs credentials.

## Development

```
cmd/fluxcd-ui/       entrypoint: flags, logging, HTTP server
internal/flux/       Flux kinds → table rows and detail views, status summarization
internal/store/      discovery, informer cache, debounced change broker, actions
internal/web/        handlers, SSE, templates, static assets, Tailwind input
charts/fluxcd-ui/    Helm chart
```

- Run `make css-watch` alongside `go run ./cmd/fluxcd-ui` while editing templates.
- The generated `internal/web/static/css/app.css` is not committed. `make build`, `make run` and the Docker build regenerate it.
- To add a kind, add an entry to `flux.Kinds` with its columns and a `describe` function. Then add its resource to the chart's ClusterRole, for both the read and the `patch` rules.
- Dependency versions are pinned in the Makefile. `make vendor-js` refreshes htmx.
