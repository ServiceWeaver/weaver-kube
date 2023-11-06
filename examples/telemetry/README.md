# Telemetry

This directory contains an example of how to use the `weaver kube` plugin API to
configure how metrics and traces are exported. In `main.go`, we register a
plugin to export traces to Jaeger and a plugin to export metrics to Prometheus.
Compile the `telemetry` binary and use it as you would `weaver kube`. Use
`prometheus.yaml` and `jaeger.yaml` to deploy Prometheus and Jaeger to a
Kubernetes cluster.

```console
$ kubectl apply -f jaeger.yaml -f prometheus.yaml -f $(telemetry deploy kube_deploy.yaml)
```
