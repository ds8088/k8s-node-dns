# k8s-node-dns

A Kubernetes controller that serves DNS A/AAAA records, which resolve to the external IPs of all ready nodes in user-defined areas.

## Purpose

I have a self-hosted Kubernetes cluster that consists of nodes at various locations.
Some of those nodes have a public IPv4, some also have IPv6 connectivity; others are NAT'ed by their home routers and cannot be reached externally.
Several nodes are running on bare-metal hardware, others are set up as VMs running in the cloud.

Most load balancing solutions cannot provide load balancing and failover for such a cluster.

However, I also run an authoritative DNS service in my cluster, and DNS can be used for this purpose like so:

- Kubernetes API server knows which nodes are alive and which are not;

- nodes can be classified into multiple "areas" (home, external, cloud and so on).
  Annotations are a good way to set the area names for each node;

- external IPs for each node are also set in the annotations.
  Since I don't have a K8s cloud provider - my Kubelets cannot set EXTERNAL-IP by themselves,
  and some of my nodes can also be NAT'ed, so I have to provide external IPs manually;

- the controller provides a builtin DNS server, which is authoritative for its own zone;

- the controller also reconciles Node objects, checks node liveness and updates the DNS records accordingly;

- my external DNS (CoreDNS) forwards requests to the controller's DNS zone, so that, for example,
  a request to home.lb.pootis.network should land at the controller's DNS server,
  and it will return all known IPs of nodes that are currently alive in the "home" area;

- a service record (`git.pootis.network`) can be then defined as a CNAME to `home.lb.pootis.network`.

## How it works

The controller watches `Node` objects in the cluster.
Each node may carry two annotations (you should set your own annotation keys, this is just an example):

- `k8s.pootis.network/node-areas` (example: `home, external`).
  A comma-separated list of area names the node belongs to.
  Each area name should be a valid DNS label.

- `k8s.pootis.network/node-ips` (example: `1.2.3.4, 2001:db8::1`).
  A comma-separated list of external IPv4/IPv6 addresses which should be advertised for this node.

The annotation keys can be changed with the `--areas-annotation` and `--ips-annotation` flags.

For zone `lb.pootis.network.` and an area `home`, querying `home.lb.pootis.network.` returns A/AAAA records for all ready nodes that list `home` in their areas annotation.
The zone apex serves SOA and NS records.

## Permissions

The controller requires read access to `Node` objects by the means of a ClusterRole:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: k8s-node-dns
rules:
  - apiGroups:
      - ""
    resources:
      - nodes
    verbs:
      - get
      - list
      - watch
```

If leader election is enabled (`--leader-elect`), the controller also needs permission to create and update `Lease` objects in its namespace.

## Helm chart

A Helm chart is available in GHCR OCI:

```sh
helm install k8s-node-dns oci://ghcr.io/ds8088/k8s-node-dns/chart/k8s-node-dns
```

[chart/values.yaml](./chart/values.yaml) contains all available configuration options.

 `config.zone`, `config.soaNS`, and `config.soaEmail` options are mandatory for Helm chart deployment.

## Running

```sh
k8s-node-dns --zone lb.pootis.network --soa-email admin@pootis.network --soa-ns ns1.lb.pootis.network:1.2.3.4
```

## Building from source

Go 1.25+ is required.

```sh
go build ./...
```

To run unit tests:

```sh
go test -race -v ./...
```

An integration test is also included, which runs the tool against a real Kubernetes API server.

To enable it, install [`setup-envtest`](https://pkg.go.dev/sigs.k8s.io/controller-runtime/tools/setup-envtest) and run:

```sh
export KUBEBUILDER_ASSETS=$(setup-envtest use -p path 1.34.1)
RUN_INTEGRATION_TESTS=1 go test ./...
```
